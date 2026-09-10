package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// ReconArtifactReader loads controller-owned reconnaissance output.
type ReconArtifactReader interface {
	ReadRecon(string, string) ([]byte, error)
}

// ReconArtifactStore persists and verifies controller-owned reconnaissance output.
type ReconArtifactStore interface {
	ReconArtifactReader
	SaveRecon(string, string, []byte) error
}

// ReconResultDecoder validates and decodes the agent's reconnaissance result.
type ReconResultDecoder interface {
	Decode([]byte) (agent.ReconOutcome, error)
}

// ReconnaissanceResult records durable state and the concise codebase artifact.
type ReconnaissanceResult struct {
	Manifest  state.Manifest
	Recon     string
	SessionID string
	Progress  []agent.ProgressEvent
}

// ReconnaissanceCreator is the workflow's standalone reconnaissance boundary.
type ReconnaissanceCreator interface {
	Recon(context.Context, string, string) (ReconnaissanceResult, error)
}

// ReconnaissanceService gathers issue-directed codebase knowledge before specification.
type ReconnaissanceService struct {
	reader     WorkflowManifestReader
	transition WorkflowTransitioner
	artifacts  ReconArtifactStore
	prompt     SpecificationPromptRenderer
	runner     agent.Runner
	decoder    ReconResultDecoder
	schemaPath string
	timeout    time.Duration
}

// NewReconnaissanceService constructs the standalone recon phase service.
func NewReconnaissanceService(
	reader WorkflowManifestReader,
	transition WorkflowTransitioner,
	artifacts ReconArtifactStore,
	promptRenderer SpecificationPromptRenderer,
	runner agent.Runner,
	decoder ReconResultDecoder,
	schemaPath string,
	timeout time.Duration,
) *ReconnaissanceService {
	return &ReconnaissanceService{
		reader: reader, transition: transition, artifacts: artifacts, prompt: promptRenderer,
		runner: runner, decoder: decoder, schemaPath: schemaPath, timeout: timeout,
	}
}

// Recon durably enters recon/running, runs a read-only agent, and persists its
// validated Markdown artifact before returning control to specification.
func (service *ReconnaissanceService) Recon(ctx context.Context, controllerRoot, workflowID string) (ReconnaissanceResult, error) {
	if err := service.validate(); err != nil {
		return ReconnaissanceResult{}, err
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return ReconnaissanceResult{}, fmt.Errorf("read persisted workflow for recon: %w", err)
	}
	if current.Phase != state.PhaseInit || current.Status != state.StatusRunning {
		return ReconnaissanceResult{}, fmt.Errorf("recon requires init/running state, got %s/%s", current.Phase, current.Status)
	}
	running := current
	running.Phase = state.PhaseRecon
	running.Status = state.StatusRunning
	running.SpecificationPath = ""
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return ReconnaissanceResult{}, fmt.Errorf("persist recon/running transition: %w", err)
	}

	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, running.Worktree)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("resolve recon worktree: %w", err))
	}
	absoluteReconPath, err := state.ReconPath(controllerRoot, workflowID)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive recon artifact path: %w", err))
	}
	relativeReconPath, err := filepath.Rel(controllerRoot, absoluteReconPath)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("derive relative recon artifact path: %w", err))
	}
	promptText, err := service.prompt.Render(reconPromptData(running, absoluteWorktree, filepath.ToSlash(relativeReconPath)))
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("render recon prompt: %w", err))
	}

	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	runResult, err := service.runner.Run(runContext, agent.Request{
		Worktree:        absoluteWorktree,
		Prompt:          promptText,
		OutputSchema:    service.schemaPath,
		OutputDirectory: filepath.Dir(absoluteReconPath),
		Access:          agent.AccessReadOnly,
	})
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("run recon agent: %w", err))
	}
	outcome, err := service.decoder.Decode(runResult.FinalOutput)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("validate recon agent result: %w", err))
	}
	if err := service.artifacts.SaveRecon(controllerRoot, workflowID, []byte(outcome.Recon)); err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("persist recon artifact: %w", err))
	}
	persistedRecon, err := service.artifacts.ReadRecon(controllerRoot, workflowID)
	if err != nil {
		return service.fail(controllerRoot, running, fmt.Errorf("verify persisted recon artifact: %w", err))
	}
	return ReconnaissanceResult{
		Manifest: running, Recon: string(persistedRecon), SessionID: runResult.SessionID, Progress: runResult.Progress,
	}, nil
}

func (service *ReconnaissanceService) validate() error {
	if service == nil || service.reader == nil || service.transition == nil || service.artifacts == nil || service.prompt == nil || service.runner == nil || service.decoder == nil {
		return errors.New("recon service is not fully configured")
	}
	if service.timeout <= 0 {
		return errors.New("recon timeout must be positive")
	}
	if !filepath.IsAbs(service.schemaPath) || filepath.Clean(service.schemaPath) != service.schemaPath {
		return errors.New("recon result schema must be an absolute clean path")
	}
	return nil
}

func (service *ReconnaissanceService) fail(controllerRoot string, running state.Manifest, cause error) (ReconnaissanceResult, error) {
	failed := running
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, failed); err != nil {
		return ReconnaissanceResult{Manifest: running}, errors.Join(cause, fmt.Errorf("persist recon failure: %w", err))
	}
	return ReconnaissanceResult{Manifest: failed}, cause
}

func reconPromptData(manifest state.Manifest, worktreePath, reconPath string) prompt.PromptData {
	return prompt.PromptData{
		WorkflowID:   manifest.WorkflowID,
		Repository:   manifest.Repository,
		Branch:       manifest.Branch,
		BaseSHA:      manifest.BaseSHA,
		WorktreePath: worktreePath,
		ReconPath:    reconPath,
		Issue: prompt.IssueData{
			Number: manifest.Issue.Number, Title: manifest.Issue.Title, Body: manifest.Issue.Body, URL: manifest.Issue.URL,
		},
	}
}
