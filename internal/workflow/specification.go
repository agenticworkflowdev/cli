package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// SpecificationManifestReader loads the durable workflow snapshot.
type SpecificationManifestReader interface {
	Read(string, string) (state.Manifest, error)
}

// SpecificationTransitioner persists validated workflow transitions.
type SpecificationTransitioner interface {
	Transition(string, string, state.Manifest) error
}

// SpecificationPromptRenderer renders a compiled specification prompt.
type SpecificationPromptRenderer interface {
	Render(prompt.PromptData) (string, error)
}

// SpecificationResultDecoder validates and decodes the agent's final output.
type SpecificationResultDecoder interface {
	Decode([]byte) (agent.Outcome, error)
}

// BlockerRequest is a typed request for the later publication phase.
type BlockerRequest struct {
	Phase    state.Phase
	Question string
}

// SpecificationResult records the durable state and agent observations.
type SpecificationResult struct {
	Manifest  state.Manifest
	SessionID string
	Progress  []agent.ProgressEvent
	Blocker   *BlockerRequest
}

// SpecificationCreator is the workflow boundary used after bootstrap.
type SpecificationCreator interface {
	Create(context.Context, string, string) (SpecificationResult, error)
}

// SpecificationService creates and verifies one workflow specification.
type SpecificationService struct {
	reader       SpecificationManifestReader
	transition   SpecificationTransitioner
	recon        ReconnaissanceCreator
	prompt       SpecificationPromptRenderer
	resumePrompt SpecificationPromptRenderer
	runner       agent.Runner
	decoder      SpecificationResultDecoder
	schemaPath   string
	timeout      time.Duration
}

// NewSpecificationService constructs the specification phase service.
func NewSpecificationService(reader SpecificationManifestReader, transition SpecificationTransitioner, recon ReconnaissanceCreator, promptRenderer SpecificationPromptRenderer, runner agent.Runner, decoder SpecificationResultDecoder, schemaPath string, timeout time.Duration, resumePrompt ...SpecificationPromptRenderer) *SpecificationService {
	service := &SpecificationService{
		reader: reader, transition: transition, recon: recon, prompt: promptRenderer,
		runner: runner, decoder: decoder, schemaPath: schemaPath, timeout: timeout,
	}
	if len(resumePrompt) > 0 {
		service.resumePrompt = resumePrompt[0]
	}
	return service
}

// Create durably enters spec/running, invokes the agent, validates its result,
// and records the specification only after the exact filesystem postcondition
// exists in the worktree.
func (service *SpecificationService) Create(ctx context.Context, controllerRoot, workflowID string) (SpecificationResult, error) {
	if err := service.validate(); err != nil {
		return SpecificationResult{}, err
	}

	reconResult, err := service.recon.Recon(ctx, controllerRoot, workflowID)
	if err != nil {
		return SpecificationResult{Manifest: reconResult.Manifest, Progress: reconResult.Progress}, err
	}
	if reconResult.Manifest.Phase != state.PhaseSpec || reconResult.Manifest.Substep != state.SubstepRecon || reconResult.Manifest.Status != state.StatusRunning {
		return SpecificationResult{Manifest: reconResult.Manifest}, errors.New("recon did not complete in spec/recon running state")
	}
	running := reconResult.Manifest
	running.Substep = state.SubstepSpecification
	running.Status = state.StatusRunning
	running.SpecificationPath = ""
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return SpecificationResult{}, fmt.Errorf("persist spec/specification transition: %w", err)
	}

	result, err := service.run(ctx, controllerRoot, running, service.prompt, reconResult.Recon)
	result.Progress = append(append([]agent.ProgressEvent(nil), reconResult.Progress...), result.Progress...)
	return result, err
}

// Resume continues specification in its prior provider session using the
// persisted blocker question and selected human answer. Legacy manifests
// without a session identity start a fresh session.
func (service *SpecificationService) Resume(ctx context.Context, controllerRoot, workflowID string) (SpecificationResult, error) {
	if err := service.validate(); err != nil {
		return SpecificationResult{}, err
	}
	if service.resumePrompt == nil {
		return SpecificationResult{}, errors.New("specification resume prompt is not configured")
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return SpecificationResult{}, fmt.Errorf("read persisted workflow for specification resume: %w", err)
	}
	if current.Phase != state.PhaseSpec || current.Substep == state.SubstepRecon || current.Status != state.StatusRunning || current.Blocker == nil || current.Blocker.Answer == nil {
		return SpecificationResult{}, fmt.Errorf("specification resume requires answered spec/running state, got %s/%s", current.Phase, current.Status)
	}
	return service.run(ctx, controllerRoot, current, service.resumePrompt, "")
}

func (service *SpecificationService) validate() error {
	if service == nil || service.reader == nil || service.transition == nil || service.recon == nil || service.prompt == nil || service.runner == nil || service.decoder == nil {
		return errors.New("specification service is not fully configured")
	}
	if service.timeout <= 0 {
		return errors.New("specification timeout must be positive")
	}
	if !filepath.IsAbs(service.schemaPath) || filepath.Clean(service.schemaPath) != service.schemaPath {
		return errors.New("specification result schema must be an absolute clean path")
	}
	return nil
}

func (service *SpecificationService) run(ctx context.Context, controllerRoot string, running state.Manifest, renderer SpecificationPromptRenderer, recon string) (SpecificationResult, error) {
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, running.Worktree)
	if err != nil {
		return SpecificationResult{}, fmt.Errorf("resolve specification worktree: %w", err)
	}
	specificationPath, err := state.SpecificationPathForBranch(running.Branch)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive specification path: %w", err))
	}
	promptText, err := renderer.Render(promptData(running, specificationPath, recon))
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("render specification prompt: %w", err))
	}
	manifestPath, err := state.ManifestPath(controllerRoot, running.WorkflowID)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive agent output directory: %w", err))
	}
	resumeSessionID, err := resumableAgentSession(running, specificationSessionRole, service.runner)
	if err != nil {
		return service.fail(controllerRoot, running, err)
	}

	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	runResult, err := service.runner.Run(runContext, agent.Request{
		Worktree:        absoluteWorktree,
		Prompt:          promptText,
		OutputSchema:    service.schemaPath,
		OutputDirectory: filepath.Dir(manifestPath),
		Access:          agent.AccessWorkspaceWrite,
		ResumeSessionID: resumeSessionID,
	})
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("run specification agent: %w", err))
	}
	outcome, err := service.decoder.Decode(runResult.FinalOutput)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("validate specification agent result: %w", err))
	}
	running, err = persistAgentSession(service.transition, controllerRoot, running, specificationSessionRole, runResult.SessionID, service.runner)
	if err != nil {
		return service.fail(controllerRoot, running, err)
	}
	result := SpecificationResult{Manifest: running, SessionID: runResult.SessionID, Progress: runResult.Progress}
	if outcome.Status == agent.OutcomeBlocked {
		result.Blocker = &BlockerRequest{Phase: state.PhaseSpec, Question: outcome.Question}
		return result, nil
	}

	if err := state.VerifySpecificationFile(absoluteWorktree, specificationPath); err != nil {
		return service.fail(controllerRoot, running, err)
	}
	completed := running
	completed.SpecificationPath = specificationPath
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, completed); err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("record verified specification: %w", err))
	}
	result.Manifest = completed
	return result, nil
}

func (service *SpecificationService) fail(controllerRoot string, running state.Manifest, cause error) (SpecificationResult, error) {
	failed := running
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, failed); err != nil {
		return SpecificationResult{Manifest: running}, errors.Join(cause, fmt.Errorf("persist specification failure: %w", err))
	}
	return SpecificationResult{Manifest: failed}, cause
}

func promptData(manifest state.Manifest, specificationPath, recon string) prompt.PromptData {
	data := prompt.PromptData{
		WorkflowID: manifest.WorkflowID, Repository: manifest.Repository, Branch: manifest.Branch,
		BaseSHA: manifest.BaseSHA, SkipSpecification: manifest.SkipSpecification, SpecificationPath: specificationPath, Recon: recon,
		Issue: prompt.IssueData{Number: manifest.Issue.Number, Title: manifest.Issue.Title, Body: manifest.Issue.Body, URL: manifest.Issue.URL},
	}
	if manifest.Blocker != nil && manifest.Blocker.Comment != nil && manifest.Blocker.Answer != nil {
		data.Blocker = &prompt.BlockerData{
			Phase: string(manifest.Blocker.Phase), Question: manifest.Blocker.Question, QuestionURL: manifest.Blocker.Comment.URL,
			Answer: manifest.Blocker.Answer.Body, AnswerAuthor: manifest.Blocker.Answer.Author, AnswerURL: manifest.Blocker.Answer.URL,
		}
	}
	return data
}

func hasRequirements(manifest state.Manifest) bool {
	return manifest.SkipSpecification || manifest.SpecificationPath != ""
}

func sanitizeTechnicalError(err error, controllerRoot string) string {
	message := strings.Map(func(character rune) rune {
		if character == '\r' || character == '\n' || unicode.IsControl(character) {
			return ' '
		}
		return character
	}, err.Error())
	message = strings.Join(strings.Fields(message), " ")
	message = state.RelativizeControllerPaths(controllerRoot, message)
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:497]) + "..."
	}
	return message
}
