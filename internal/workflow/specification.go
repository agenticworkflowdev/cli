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
	reader     SpecificationManifestReader
	transition SpecificationTransitioner
	prompt     SpecificationPromptRenderer
	runner     agent.Runner
	decoder    SpecificationResultDecoder
	schemaPath string
	timeout    time.Duration
}

// NewSpecificationService constructs the specification phase service.
func NewSpecificationService(reader SpecificationManifestReader, transition SpecificationTransitioner, promptRenderer SpecificationPromptRenderer, runner agent.Runner, decoder SpecificationResultDecoder, schemaPath string, timeout time.Duration) *SpecificationService {
	return &SpecificationService{
		reader: reader, transition: transition, prompt: promptRenderer,
		runner: runner, decoder: decoder, schemaPath: schemaPath, timeout: timeout,
	}
}

// Create durably enters spec/running, invokes the agent, validates its result,
// and records the specification only after the exact filesystem postcondition
// exists in the worktree.
func (service *SpecificationService) Create(ctx context.Context, controllerRoot, workflowID string) (SpecificationResult, error) {
	if service == nil || service.reader == nil || service.transition == nil || service.prompt == nil || service.runner == nil || service.decoder == nil {
		return SpecificationResult{}, errors.New("specification service is not fully configured")
	}
	if service.timeout <= 0 {
		return SpecificationResult{}, errors.New("specification timeout must be positive")
	}
	if !filepath.IsAbs(service.schemaPath) || filepath.Clean(service.schemaPath) != service.schemaPath {
		return SpecificationResult{}, errors.New("specification result schema must be an absolute clean path")
	}

	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return SpecificationResult{}, fmt.Errorf("read persisted workflow for specification: %w", err)
	}
	if current.Phase != state.PhaseInit || current.Status != state.StatusRunning {
		return SpecificationResult{}, fmt.Errorf("specification requires init/running state, got %s/%s", current.Phase, current.Status)
	}
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, current.Worktree)
	if err != nil {
		return SpecificationResult{}, fmt.Errorf("resolve specification worktree: %w", err)
	}

	running := current
	running.Phase = state.PhaseSpec
	running.Status = state.StatusRunning
	running.SpecificationPath = ""
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return SpecificationResult{}, fmt.Errorf("persist spec/running transition: %w", err)
	}

	specificationPath, err := state.SpecificationPathForBranch(running.Branch)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive specification path: %w", err))
	}
	promptText, err := service.prompt.Render(prompt.PromptData{
		WorkflowID:        running.WorkflowID,
		Repository:        running.Repository,
		Branch:            running.Branch,
		BaseSHA:           running.BaseSHA,
		SpecificationPath: specificationPath,
		Issue: prompt.IssueData{
			Number: running.Issue.Number,
			Title:  running.Issue.Title,
			Body:   running.Issue.Body,
			URL:    running.Issue.URL,
		},
	})
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("render specification prompt: %w", err))
	}
	manifestPath, err := state.ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive agent output directory: %w", err))
	}

	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	runResult, err := service.runner.Run(runContext, agent.Request{
		Worktree:        absoluteWorktree,
		Prompt:          promptText,
		OutputSchema:    service.schemaPath,
		OutputDirectory: filepath.Dir(manifestPath),
		Access:          agent.AccessWorkspaceWrite,
	})
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("run specification agent: %w", err))
	}
	outcome, err := service.decoder.Decode(runResult.FinalOutput)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("validate specification agent result: %w", err))
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
	if err := service.transition.Transition(controllerRoot, workflowID, completed); err != nil {
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
