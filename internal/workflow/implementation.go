package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/checks"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
)

const maxCheckFixInvocations = 3

// ImplementationResult records the current durable state, agent observations,
// and check evidence describing the current worktree diff.
type ImplementationResult struct {
	Manifest     state.Manifest
	SessionIDs   []string
	Progress     []agent.ProgressEvent
	CheckResults []checks.Result
	CheckedState gitrepo.WorktreeBaseline
	Blocker      *BlockerRequest
}

// ImplementationFailure retains bounded check evidence for the command-level
// private diagnostic report while presenting a concise error to the user.
type ImplementationFailure struct {
	Cause        error
	CheckResults []checks.Result
}

func (failure *ImplementationFailure) Error() string {
	if failure == nil || failure.Cause == nil {
		return "implementation failed"
	}
	return failure.Cause.Error()
}

func (failure *ImplementationFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// DiagnosticDetails exposes bounded process and check evidence to the
// existing private diagnostic-log writer.
func (failure *ImplementationFailure) DiagnosticDetails() string {
	if failure == nil {
		return ""
	}
	var details strings.Builder
	var underlying interface{ DiagnosticDetails() string }
	if errors.As(failure.Cause, &underlying) {
		if value := strings.TrimSpace(underlying.DiagnosticDetails()); value != "" {
			details.WriteString(value)
			details.WriteByte('\n')
		}
	}
	for index, result := range failure.CheckResults {
		fmt.Fprintf(&details, "check_result_%d:\n", index+1)
		details.WriteString(result.DiagnosticDetails())
		details.WriteByte('\n')
	}
	return strings.TrimSpace(details.String())
}

// ImplementationRunner is the workflow boundary used after requirements are ready.
type ImplementationRunner interface {
	Implement(context.Context, string, string) (ImplementationResult, error)
}

// ImplementationService orchestrates implementation, deterministic checks,
// and a fixed bounded check-repair loop.
type ImplementationService struct {
	reader          SpecificationManifestReader
	transition      SpecificationTransitioner
	implementPrompt SpecificationPromptRenderer
	repairPrompt    SpecificationPromptRenderer
	resumePrompt    SpecificationPromptRenderer
	runner          agent.Runner
	decoder         SpecificationResultDecoder
	checks          checks.Runner
	diff            gitrepo.DiffScopeInspector
	worktree        gitrepo.WorktreeStateInspector
	schemaPath      string
	timeout         time.Duration
	definitions     []checks.Definition
	protectedPaths  []string
}

// NewImplementationService constructs the implementation phase service.
func NewImplementationService(
	reader SpecificationManifestReader,
	transition SpecificationTransitioner,
	implementPrompt SpecificationPromptRenderer,
	repairPrompt SpecificationPromptRenderer,
	runner agent.Runner,
	decoder SpecificationResultDecoder,
	checkRunner checks.Runner,
	diff gitrepo.DiffScopeInspector,
	worktree gitrepo.WorktreeStateInspector,
	schemaPath string,
	timeout time.Duration,
	definitions []checks.Definition,
	protectedPaths []string,
	resumePrompt ...SpecificationPromptRenderer,
) *ImplementationService {
	service := &ImplementationService{
		reader: reader, transition: transition, implementPrompt: implementPrompt, repairPrompt: repairPrompt,
		runner: runner, decoder: decoder, checks: checkRunner, diff: diff, worktree: worktree, schemaPath: schemaPath, timeout: timeout,
		definitions: append([]checks.Definition(nil), definitions...), protectedPaths: append([]string(nil), protectedPaths...),
	}
	if len(resumePrompt) > 0 {
		service.resumePrompt = resumePrompt[0]
	}
	return service
}

// Implement durably enters implementation/running before granting workspace
// write access. Passing evidence is returned only for the current post-agent
// diff; every repair reruns the complete check set.
func (service *ImplementationService) Implement(ctx context.Context, controllerRoot, workflowID string) (ImplementationResult, error) {
	if err := service.validate(); err != nil {
		return ImplementationResult{}, err
	}

	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return ImplementationResult{}, fmt.Errorf("read persisted workflow for implementation: %w", err)
	}
	readyWithSpecification := !current.SkipSpecification && current.Phase == state.PhaseSpec && current.SpecificationPath != ""
	readyWithoutSpecification := current.SkipSpecification && current.Phase == state.PhaseInit && current.SpecificationPath == ""
	if current.Status != state.StatusRunning || (!readyWithSpecification && !readyWithoutSpecification) {
		return ImplementationResult{}, fmt.Errorf("implementation requires completed spec/running state or init/running with specification skipped, got %s/%s", current.Phase, current.Status)
	}
	running := current
	running.Phase = state.PhaseImplementation
	running.Status = state.StatusRunning
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return ImplementationResult{}, fmt.Errorf("persist implementation/running transition: %w", err)
	}
	return service.run(ctx, controllerRoot, running, service.implementPrompt)
}

// Resume continues implementation in a fresh agent run, then repeats the
// complete deterministic check and bounded repair cycle.
func (service *ImplementationService) Resume(ctx context.Context, controllerRoot, workflowID string) (ImplementationResult, error) {
	if err := service.validate(); err != nil {
		return ImplementationResult{}, err
	}
	if service.resumePrompt == nil {
		return ImplementationResult{}, errors.New("implementation resume prompt is not configured")
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return ImplementationResult{}, fmt.Errorf("read persisted workflow for implementation resume: %w", err)
	}
	if current.Phase != state.PhaseImplementation || current.Status != state.StatusRunning || !hasRequirements(current) || current.Blocker == nil || current.Blocker.Answer == nil {
		return ImplementationResult{}, fmt.Errorf("implementation resume requires answered implementation/running state, got %s/%s", current.Phase, current.Status)
	}
	return service.run(ctx, controllerRoot, current, service.resumePrompt)
}

func (service *ImplementationService) validate() error {
	if service == nil || service.reader == nil || service.transition == nil || service.implementPrompt == nil || service.repairPrompt == nil || service.runner == nil || service.decoder == nil || service.checks == nil || service.diff == nil || service.worktree == nil {
		return errors.New("implementation service is not fully configured")
	}
	if service.timeout <= 0 {
		return errors.New("implementation agent timeout must be positive")
	}
	if !filepath.IsAbs(service.schemaPath) || filepath.Clean(service.schemaPath) != service.schemaPath {
		return errors.New("implementation result schema must be an absolute clean path")
	}
	return nil
}

func (service *ImplementationService) run(ctx context.Context, controllerRoot string, running state.Manifest, renderer SpecificationPromptRenderer) (ImplementationResult, error) {
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, running.Worktree)
	if err != nil {
		return ImplementationResult{}, fmt.Errorf("resolve implementation worktree: %w", err)
	}

	manifestPath, err := state.ManifestPath(controllerRoot, running.WorkflowID)
	if err != nil {
		return service.fail(controllerRoot, running, ImplementationResult{Manifest: running}, fmt.Errorf("derive implementation agent output directory: %w", err))
	}
	basePromptData := promptData(running, running.SpecificationPath)
	result := ImplementationResult{Manifest: running}

	outcome, runResult, err := service.runAgent(ctx, controllerRoot, absoluteWorktree, filepath.Dir(manifestPath), renderer, basePromptData)
	result.observe(runResult)
	if err != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("run implementation agent: %w", err))
	}
	if outcome.Status == agent.OutcomeBlocked {
		result.Blocker = &BlockerRequest{Phase: state.PhaseImplementation, Question: outcome.Question}
		return result, nil
	}

	for repairAttempt := 0; ; repairAttempt++ {
		checkedState, captureErr := service.worktree.Capture(ctx, absoluteWorktree)
		if captureErr != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("capture pre-check worktree state: %w", captureErr))
		}
		checkResults, checkErr := service.checks.Run(ctx, absoluteWorktree, service.definitions)
		result.CheckResults = cloneCheckResults(checkResults)
		if checkErr != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("run deterministic checks: %w", checkErr))
		}
		changed, inspectErr := service.worktree.Inspect(ctx, absoluteWorktree, checkedState)
		if inspectErr != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("verify post-check worktree state: %w", inspectErr))
		}
		if len(changed) > 0 {
			return service.fail(controllerRoot, running, result, fmt.Errorf("deterministic checks changed worktree paths: %s", strings.Join(changed, ", ")))
		}
		failed := failedCheckResults(checkResults)
		if len(failed) == 0 {
			result.CheckedState = checkedState
			return result, nil
		}
		if repairAttempt == maxCheckFixInvocations {
			return service.fail(controllerRoot, running, result, fmt.Errorf("deterministic checks still failed after %d repair attempts", maxCheckFixInvocations))
		}

		repairData := basePromptData
		repairData.CheckResults = failed
		outcome, runResult, err = service.runAgent(ctx, controllerRoot, absoluteWorktree, filepath.Dir(manifestPath), service.repairPrompt, repairData)
		result.observe(runResult)
		if err != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("run check-fix agent: %w", err))
		}
		if outcome.Status == agent.OutcomeBlocked {
			result.Blocker = &BlockerRequest{Phase: state.PhaseImplementation, Question: outcome.Question}
			result.CheckResults = nil
			return result, nil
		}
		// The repair changed the diff, so the prior evidence is no longer valid.
		result.CheckResults = nil
	}
}

func (result *ImplementationResult) observe(runResult agent.RunResult) {
	if runResult.SessionID != "" {
		result.SessionIDs = append(result.SessionIDs, runResult.SessionID)
	}
	result.Progress = append(result.Progress, runResult.Progress...)
}

func (service *ImplementationService) runAgent(
	ctx context.Context,
	controllerRoot, worktree, outputDirectory string,
	renderer SpecificationPromptRenderer,
	data prompt.PromptData,
) (agent.Outcome, agent.RunResult, error) {
	promptText, err := renderer.Render(data)
	if err != nil {
		return agent.Outcome{}, agent.RunResult{}, fmt.Errorf("render agent prompt: %w", err)
	}
	diffScope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree, ProtectedPrefixes: service.protectedPaths}
	baseline, err := service.diff.Capture(diffScope)
	if err != nil {
		return agent.Outcome{}, agent.RunResult{}, fmt.Errorf("capture pre-agent diff scope: %w", err)
	}

	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	runResult, runErr := service.runner.Run(runContext, agent.Request{
		Worktree: worktree, Prompt: promptText, OutputSchema: service.schemaPath,
		OutputDirectory: outputDirectory, Access: agent.AccessWorkspaceWrite,
	})
	cancel()
	offending, inspectErr := service.diff.Inspect(ctx, diffScope, baseline)
	if inspectErr != nil {
		return agent.Outcome{}, runResult, fmt.Errorf("inspect post-agent diff scope: %w", inspectErr)
	}
	if len(offending) > 0 {
		return agent.Outcome{}, runResult, fmt.Errorf("implementation agent changed protected paths: %s", strings.Join(offending, ", "))
	}
	if runErr != nil {
		return agent.Outcome{}, runResult, runErr
	}
	outcome, err := service.decoder.Decode(runResult.FinalOutput)
	if err != nil {
		return agent.Outcome{}, runResult, fmt.Errorf("validate implementation agent result: %w", err)
	}
	return outcome, runResult, nil
}

func failedCheckResults(results []checks.Result) []checks.Result {
	failed := make([]checks.Result, 0)
	for _, result := range results {
		if !result.Passed() {
			failed = append(failed, cloneCheckResult(result))
		}
	}
	return failed
}

func cloneCheckResults(results []checks.Result) []checks.Result {
	cloned := make([]checks.Result, len(results))
	for index, result := range results {
		cloned[index] = cloneCheckResult(result)
	}
	return cloned
}

func cloneCheckResult(result checks.Result) checks.Result {
	result.Command = append([]string(nil), result.Command...)
	return result
}

func (service *ImplementationService) fail(controllerRoot string, running state.Manifest, result ImplementationResult, cause error) (ImplementationResult, error) {
	failed := running
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, failed); err != nil {
		result.Manifest = running
		return result, &ImplementationFailure{Cause: errors.Join(cause, fmt.Errorf("persist implementation failure: %w", err)), CheckResults: cloneCheckResults(result.CheckResults)}
	}
	result.Manifest = failed
	return result, &ImplementationFailure{Cause: cause, CheckResults: cloneCheckResults(result.CheckResults)}
}
