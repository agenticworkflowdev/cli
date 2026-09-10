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
	"github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// ReviewResult records the latest current evidence and observations from the
// independent review/correction loop.
type ReviewResult struct {
	Manifest     state.Manifest
	SessionIDs   []string
	Progress     []agent.ProgressEvent
	CheckResults []checks.Result
	Evidence     *review.Result
	Blocker      *BlockerRequest
	CheckedState gitrepo.WorktreeBaseline
}

// ReviewFailure retains bounded check evidence for private diagnostics.
type ReviewFailure struct {
	Cause        error
	CheckResults []checks.Result
}

func (failure *ReviewFailure) Error() string {
	if failure == nil || failure.Cause == nil {
		return "review failed"
	}
	return failure.Cause.Error()
}

func (failure *ReviewFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// DiagnosticDetails exposes current bounded check evidence to the diagnostic
// log writer.
func (failure *ReviewFailure) DiagnosticDetails() string {
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

// Reviewer is the workflow boundary used after implementation checks pass.
type Reviewer interface {
	Review(context.Context, string, string, []checks.Result, gitrepo.WorktreeBaseline) (ReviewResult, error)
}

// ReviewResultDecoder validates an independent review agent result.
type ReviewResultDecoder interface {
	Decode([]byte) (review.Result, error)
}

// ReviewEvidenceWriter atomically persists the latest complete review result.
type ReviewEvidenceWriter interface {
	SaveReview(string, string, review.Result) error
	InvalidateReview(string, string) error
}

// ReviewService runs bounded independent reviews and corrections.
type ReviewService struct {
	reader           SpecificationManifestReader
	transition       SpecificationTransitioner
	reviewPrompt     SpecificationPromptRenderer
	correctionPrompt SpecificationPromptRenderer
	resumePrompt     SpecificationPromptRenderer
	runner           agent.Runner
	reviewDecoder    ReviewResultDecoder
	outcomeDecoder   SpecificationResultDecoder
	checks           checks.Runner
	diff             gitrepo.DiffScopeInspector
	worktree         gitrepo.WorktreeStateInspector
	evidence         ReviewEvidenceWriter
	reviewSchemaPath string
	agentSchemaPath  string
	timeout          time.Duration
	definitions      []checks.Definition
	protectedPaths   []string
	maxAttempts      int
}

// NewReviewService constructs the independent review/correction service.
func NewReviewService(
	reader SpecificationManifestReader,
	transition SpecificationTransitioner,
	reviewPrompt SpecificationPromptRenderer,
	correctionPrompt SpecificationPromptRenderer,
	runner agent.Runner,
	reviewDecoder ReviewResultDecoder,
	outcomeDecoder SpecificationResultDecoder,
	checkRunner checks.Runner,
	diff gitrepo.DiffScopeInspector,
	worktree gitrepo.WorktreeStateInspector,
	evidence ReviewEvidenceWriter,
	reviewSchemaPath string,
	agentSchemaPath string,
	timeout time.Duration,
	definitions []checks.Definition,
	protectedPaths []string,
	maxAttempts int,
	resumePrompt ...SpecificationPromptRenderer,
) *ReviewService {
	service := &ReviewService{
		reader: reader, transition: transition, reviewPrompt: reviewPrompt, correctionPrompt: correctionPrompt,
		runner: runner, reviewDecoder: reviewDecoder, outcomeDecoder: outcomeDecoder, checks: checkRunner,
		diff: diff, worktree: worktree, evidence: evidence, reviewSchemaPath: reviewSchemaPath,
		agentSchemaPath: agentSchemaPath, timeout: timeout, definitions: append([]checks.Definition(nil), definitions...),
		protectedPaths: append([]string(nil), protectedPaths...), maxAttempts: maxAttempts,
	}
	if len(resumePrompt) > 0 {
		service.resumePrompt = resumePrompt[0]
	}
	return service
}

// Review enters review/running only for the diff described by passing check
// evidence, then repeats correction, all checks, and a fresh review as needed.
func (service *ReviewService) Review(ctx context.Context, controllerRoot, workflowID string, checkResults []checks.Result, checkedState gitrepo.WorktreeBaseline) (ReviewResult, error) {
	if err := service.validate(); err != nil {
		return ReviewResult{}, err
	}
	for _, result := range checkResults {
		if !result.Passed() {
			return ReviewResult{}, fmt.Errorf("review requires passing deterministic check evidence; check %q did not pass", result.Name)
		}
	}

	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("read persisted workflow for review: %w", err)
	}
	if current.Phase != state.PhaseImplementation || current.Status != state.StatusRunning || !hasRequirements(current) {
		return ReviewResult{}, fmt.Errorf("review requires checked implementation/running state, got %s/%s", current.Phase, current.Status)
	}
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, current.Worktree)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("resolve review worktree: %w", err)
	}
	if changed, err := service.worktree.Inspect(ctx, absoluteWorktree, checkedState); err != nil {
		initial := ReviewResult{Manifest: current, CheckResults: cloneCheckResults(checkResults)}
		return service.fail(controllerRoot, current, initial, fmt.Errorf("verify checked worktree state: %w", err))
	} else if len(changed) > 0 {
		initial := ReviewResult{Manifest: current, CheckResults: cloneCheckResults(checkResults)}
		return service.fail(controllerRoot, current, initial, fmt.Errorf("worktree changed after deterministic checks: %s", strings.Join(changed, ", ")))
	}
	manifestPath, err := state.ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("derive review agent output directory: %w", err)
	}
	outputDirectory := filepath.Dir(manifestPath)

	running := current
	running.Phase = state.PhaseReview
	if running.Review == nil {
		running.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: service.maxAttempts}
	} else {
		reviewCounters := *running.Review
		running.Review = &reviewCounters
	}
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return ReviewResult{}, fmt.Errorf("persist review/running transition: %w", err)
	}

	baseData := promptData(running, running.SpecificationPath)
	result := ReviewResult{Manifest: running, CheckResults: cloneCheckResults(checkResults)}
	return service.reviewLoop(ctx, controllerRoot, outputDirectory, absoluteWorktree, baseData, result, checkedState)
}

func (service *ReviewService) reviewLoop(ctx context.Context, controllerRoot, outputDirectory, absoluteWorktree string, baseData prompt.PromptData, result ReviewResult, checkedState gitrepo.WorktreeBaseline) (ReviewResult, error) {
	workflowID := result.Manifest.WorkflowID
	for {
		if changed, inspectErr := service.worktree.Inspect(ctx, absoluteWorktree, checkedState); inspectErr != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("verify checked worktree before review: %w", inspectErr))
		} else if len(changed) > 0 {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("worktree changed after deterministic checks: %s", strings.Join(changed, ", ")))
		}
		reviewData := baseData
		reviewData.CheckResults = checkResultsForReview(result.CheckResults)
		evidence, runResult, err := service.runReadOnlyReview(ctx, absoluteWorktree, outputDirectory, reviewData, checkedState)
		result.observe(runResult)
		if err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("run independent review: %w", err))
		}
		if err := service.evidence.SaveReview(controllerRoot, workflowID, evidence); err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("persist review evidence: %w", err))
		}
		persisted := cloneReviewResult(evidence)
		result.Evidence = &persisted
		if evidence.Approved {
			result.CheckedState = checkedState
			return result, nil
		}
		if result.Manifest.Review.Attempt >= result.Manifest.Review.MaxAttempts {
			result.Blocker = &BlockerRequest{Phase: state.PhaseReview, Question: reviewDirectionQuestion(evidence, result.Manifest.Review.Attempt)}
			return result, nil
		}
		if err := service.evidence.InvalidateReview(controllerRoot, workflowID); err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("invalidate review evidence before correction: %w", err))
		}
		result.Evidence = nil
		result.CheckResults = nil

		correcting := result.Manifest
		correcting.Phase = state.PhaseImplementation
		correcting.Review = &state.ReviewCounters{Attempt: result.Manifest.Review.Attempt + 1, MaxAttempts: result.Manifest.Review.MaxAttempts}
		if err := service.transition.Transition(controllerRoot, workflowID, correcting); err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("persist review correction transition: %w", err))
		}
		result.Manifest = correcting
		correctionData := baseData
		correctionData.ReviewFindings = cloneFindings(evidence.Findings)
		resumeSessionID, resumeErr := resumableAgentSession(result.Manifest, implementationSessionRole, service.runner)
		if resumeErr != nil {
			return service.fail(controllerRoot, result.Manifest, result, resumeErr)
		}
		outcome, correctionRun, err := service.runCorrection(ctx, controllerRoot, absoluteWorktree, outputDirectory, service.correctionPrompt, correctionData, resumeSessionID)
		result.observe(correctionRun)
		if err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("run review correction agent: %w", err))
		}
		correcting, err = persistAgentSession(service.transition, controllerRoot, correcting, implementationSessionRole, correctionRun.SessionID, service.runner)
		if err != nil {
			return service.fail(controllerRoot, result.Manifest, result, err)
		}
		result.Manifest = correcting
		if outcome.Status == agent.OutcomeBlocked {
			result.Blocker = &BlockerRequest{Phase: state.PhaseImplementation, Question: outcome.Question}
			return result, nil
		}

		nextCheckedState, captureErr := service.worktree.Capture(ctx, absoluteWorktree)
		if captureErr != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("capture pre-check corrected worktree state: %w", captureErr))
		}
		currentChecks, checkErr := service.checks.Run(ctx, absoluteWorktree, service.definitions)
		result.CheckResults = cloneCheckResults(currentChecks)
		if checkErr != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("run deterministic checks after review correction: %w", checkErr))
		}
		if changed, inspectErr := service.worktree.Inspect(ctx, absoluteWorktree, nextCheckedState); inspectErr != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("verify corrected checked worktree state: %w", inspectErr))
		} else if len(changed) > 0 {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("deterministic checks changed corrected worktree paths: %s", strings.Join(changed, ", ")))
		}
		if failed := failedCheckResults(currentChecks); len(failed) > 0 {
			return service.fail(controllerRoot, result.Manifest, result, errors.New("deterministic checks failed after review correction"))
		}
		checkedState = nextCheckedState

		rereview := result.Manifest
		rereview.Phase = state.PhaseReview
		if err := service.transition.Transition(controllerRoot, workflowID, rereview); err != nil {
			return service.fail(controllerRoot, result.Manifest, result, fmt.Errorf("persist fresh review transition: %w", err))
		}
		result.Manifest = rereview
	}
}

// Resume applies the persisted human direction through the implementation
// session, reruns deterministic checks, and starts a fresh independent review
// without resetting the bounded review attempt counter.
func (service *ReviewService) Resume(ctx context.Context, controllerRoot, workflowID string) (ReviewResult, error) {
	if err := service.validate(); err != nil {
		return ReviewResult{}, err
	}
	if service.resumePrompt == nil {
		return ReviewResult{}, errors.New("review resume prompt is not configured")
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("read persisted workflow for review resume: %w", err)
	}
	if current.Phase != state.PhaseReview || current.Status != state.StatusRunning || !hasRequirements(current) || current.Review == nil || current.Blocker == nil || current.Blocker.Answer == nil {
		return ReviewResult{}, fmt.Errorf("review resume requires answered review/running state, got %s/%s", current.Phase, current.Status)
	}
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, current.Worktree)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("resolve review resume worktree: %w", err)
	}
	manifestPath, err := state.ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("derive review resume output directory: %w", err)
	}
	result := ReviewResult{Manifest: current}
	if err := service.evidence.InvalidateReview(controllerRoot, workflowID); err != nil {
		return service.fail(controllerRoot, current, result, fmt.Errorf("invalidate review evidence before human-directed correction: %w", err))
	}
	baseData := promptData(current, current.SpecificationPath)
	resumeSessionID, err := resumableAgentSession(current, implementationSessionRole, service.runner)
	if err != nil {
		return service.fail(controllerRoot, current, result, err)
	}
	outcome, runResult, err := service.runCorrection(ctx, controllerRoot, absoluteWorktree, filepath.Dir(manifestPath), service.resumePrompt, baseData, resumeSessionID)
	result.observe(runResult)
	if err != nil {
		return service.fail(controllerRoot, current, result, fmt.Errorf("run human-directed review correction: %w", err))
	}
	current, err = persistAgentSession(service.transition, controllerRoot, current, implementationSessionRole, runResult.SessionID, service.runner)
	if err != nil {
		return service.fail(controllerRoot, current, result, err)
	}
	result.Manifest = current
	if outcome.Status == agent.OutcomeBlocked {
		result.Blocker = &BlockerRequest{Phase: state.PhaseReview, Question: outcome.Question}
		return result, nil
	}
	checkedState, err := service.worktree.Capture(ctx, absoluteWorktree)
	if err != nil {
		return service.fail(controllerRoot, current, result, fmt.Errorf("capture pre-check resumed worktree state: %w", err))
	}
	checkResults, err := service.checks.Run(ctx, absoluteWorktree, service.definitions)
	result.CheckResults = cloneCheckResults(checkResults)
	if err != nil {
		return service.fail(controllerRoot, current, result, fmt.Errorf("run deterministic checks after human direction: %w", err))
	}
	if changed, inspectErr := service.worktree.Inspect(ctx, absoluteWorktree, checkedState); inspectErr != nil {
		return service.fail(controllerRoot, current, result, fmt.Errorf("verify resumed checked worktree state: %w", inspectErr))
	} else if len(changed) > 0 {
		return service.fail(controllerRoot, current, result, fmt.Errorf("deterministic checks changed resumed worktree paths: %s", strings.Join(changed, ", ")))
	}
	if failed := failedCheckResults(checkResults); len(failed) > 0 {
		return service.fail(controllerRoot, current, result, errors.New("deterministic checks failed after human direction"))
	}
	return service.reviewLoop(ctx, controllerRoot, filepath.Dir(manifestPath), absoluteWorktree, baseData, result, checkedState)
}

func (service *ReviewService) validate() error {
	if service == nil || service.reader == nil || service.transition == nil || service.reviewPrompt == nil || service.correctionPrompt == nil || service.runner == nil || service.reviewDecoder == nil || service.outcomeDecoder == nil || service.checks == nil || service.diff == nil || service.worktree == nil || service.evidence == nil {
		return errors.New("review service is not fully configured")
	}
	if service.timeout <= 0 {
		return errors.New("review agent timeout must be positive")
	}
	for name, value := range map[string]string{"review result schema": service.reviewSchemaPath, "agent result schema": service.agentSchemaPath} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	if service.maxAttempts < 1 || service.maxAttempts > 10 {
		return errors.New("review max attempts must be between 1 and 10")
	}
	return nil
}

func (service *ReviewService) runReadOnlyReview(ctx context.Context, worktree, outputDirectory string, data prompt.PromptData, checkedState gitrepo.WorktreeBaseline) (review.Result, agent.RunResult, error) {
	promptText, err := service.reviewPrompt.Render(data)
	if err != nil {
		return review.Result{}, agent.RunResult{}, fmt.Errorf("render review prompt: %w", err)
	}
	baseline, err := service.worktree.Capture(ctx, worktree)
	if err != nil {
		return review.Result{}, agent.RunResult{}, fmt.Errorf("capture pre-review worktree: %w", err)
	}
	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	runResult, runErr := service.runner.Run(runContext, agent.Request{
		Worktree: worktree, Prompt: promptText, OutputSchema: service.reviewSchemaPath,
		OutputDirectory: outputDirectory, Access: agent.AccessReadOnly,
	})
	cancel()
	inspectContext, inspectCancel := context.WithTimeout(context.WithoutCancel(ctx), service.timeout)
	changed, inspectErr := service.worktree.Inspect(inspectContext, worktree, baseline)
	if inspectErr != nil {
		inspectCancel()
		return review.Result{}, runResult, fmt.Errorf("inspect post-review worktree: %w", inspectErr)
	}
	if len(changed) > 0 {
		inspectCancel()
		return review.Result{}, runResult, fmt.Errorf("read-only review changed worktree paths: %s", strings.Join(changed, ", "))
	}
	checkedChanged, checkedInspectErr := service.worktree.Inspect(inspectContext, worktree, checkedState)
	inspectCancel()
	if checkedInspectErr != nil {
		return review.Result{}, runResult, fmt.Errorf("compare reviewed worktree with checked state: %w", checkedInspectErr)
	}
	if len(checkedChanged) > 0 {
		return review.Result{}, runResult, fmt.Errorf("reviewed worktree differs from checked state: %s", strings.Join(checkedChanged, ", "))
	}
	if runErr != nil {
		return review.Result{}, runResult, runErr
	}
	evidence, err := service.reviewDecoder.Decode(runResult.FinalOutput)
	if err != nil {
		return review.Result{}, runResult, fmt.Errorf("validate review agent result: %w", err)
	}
	return evidence, runResult, nil
}

func (service *ReviewService) runCorrection(ctx context.Context, controllerRoot, worktree, outputDirectory string, renderer SpecificationPromptRenderer, data prompt.PromptData, resumeSessionID string) (agent.Outcome, agent.RunResult, error) {
	promptText, err := renderer.Render(data)
	if err != nil {
		return agent.Outcome{}, agent.RunResult{}, fmt.Errorf("render correction prompt: %w", err)
	}
	scope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree, ProtectedPrefixes: service.protectedPaths}
	baseline, err := service.diff.Capture(scope)
	if err != nil {
		return agent.Outcome{}, agent.RunResult{}, fmt.Errorf("capture pre-correction diff scope: %w", err)
	}
	runContext, cancel := context.WithTimeout(ctx, service.timeout)
	runResult, runErr := service.runner.Run(runContext, agent.Request{
		Worktree: worktree, Prompt: promptText, OutputSchema: service.agentSchemaPath,
		OutputDirectory: outputDirectory, Access: agent.AccessWorkspaceWrite, ResumeSessionID: resumeSessionID,
	})
	cancel()
	offending, inspectErr := service.diff.Inspect(ctx, scope, baseline)
	if inspectErr != nil {
		return agent.Outcome{}, runResult, fmt.Errorf("inspect post-correction diff scope: %w", inspectErr)
	}
	if len(offending) > 0 {
		return agent.Outcome{}, runResult, fmt.Errorf("review correction changed protected paths: %s", strings.Join(offending, ", "))
	}
	if runErr != nil {
		return agent.Outcome{}, runResult, runErr
	}
	outcome, err := service.outcomeDecoder.Decode(runResult.FinalOutput)
	if err != nil {
		return agent.Outcome{}, runResult, fmt.Errorf("validate review correction result: %w", err)
	}
	return outcome, runResult, nil
}

func (result *ReviewResult) observe(runResult agent.RunResult) {
	if runResult.SessionID != "" {
		result.SessionIDs = append(result.SessionIDs, runResult.SessionID)
	}
	result.Progress = append(result.Progress, runResult.Progress...)
}

func (service *ReviewService) fail(controllerRoot string, running state.Manifest, result ReviewResult, cause error) (ReviewResult, error) {
	failed := running
	failed.Status = state.StatusFailed
	failed.Blocker = nil
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, failed); err != nil {
		result.Manifest = running
		result.Evidence = nil
		return result, &ReviewFailure{Cause: errors.Join(cause, fmt.Errorf("persist review failure: %w", err)), CheckResults: cloneCheckResults(result.CheckResults)}
	}
	result.Manifest = failed
	result.Evidence = nil
	return result, &ReviewFailure{Cause: cause, CheckResults: cloneCheckResults(result.CheckResults)}
}

func cloneReviewResult(result review.Result) review.Result {
	result.Findings = cloneFindings(result.Findings)
	return result
}

func cloneFindings(findings []review.Finding) []review.Finding {
	return append([]review.Finding(nil), findings...)
}

func reviewDirectionQuestion(result review.Result, attempt int) string {
	first := result.Findings[0]
	location := first.Path
	if first.Line > 0 {
		location = fmt.Sprintf("%s:%d", first.Path, first.Line)
	}
	if location != "" {
		location += ": "
	}
	return fmt.Sprintf("Independent review attempt %d still has %d actionable finding(s). What direction should the implementation take? First finding: [%s] %s%s", attempt, len(result.Findings), first.Severity, location, first.Message)
}
