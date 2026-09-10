package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agenticworkflowdev/cli/internal/checks"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// ReviewEvidenceReader reads the durable independent review decision.
type ReviewEvidenceReader interface {
	ReadReview(string, string) (review.Result, error)
}

// PullRequestGateway is the external PR reconciliation boundary.
type PullRequestGateway interface {
	ListPullRequests(context.Context, string, string, string) ([]githubapi.PullRequest, error)
	CreatePullRequest(context.Context, string, githubapi.CreatePullRequestRequest) (githubapi.PullRequest, error)
}

// PublicationResult records final check evidence and durable terminal state.
type PublicationResult struct {
	Manifest     state.Manifest
	CheckResults []checks.Result
}

// PublicationFailure preserves bounded final-check diagnostics.
type PublicationFailure struct {
	Cause        error
	CheckResults []checks.Result
}

func (failure *PublicationFailure) Error() string {
	if failure == nil || failure.Cause == nil {
		return "pull request publication failed"
	}
	return failure.Cause.Error()
}

func (failure *PublicationFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

func (failure *PublicationFailure) DiagnosticDetails() string {
	if failure == nil {
		return ""
	}
	var details strings.Builder
	var underlying interface{ DiagnosticDetails() string }
	if errors.As(failure.Cause, &underlying) {
		details.WriteString(strings.TrimSpace(underlying.DiagnosticDetails()))
		details.WriteByte('\n')
	}
	for index, result := range failure.CheckResults {
		fmt.Fprintf(&details, "check_result_%d:\n%s\n", index+1, result.DiagnosticDetails())
	}
	return strings.TrimSpace(details.String())
}

// Finalizer is the terminal workflow boundary used by run, resume, and
// interrupted publication re-entry.
type Finalizer interface {
	Finalize(context.Context, string, string, gitrepo.WorktreeBaseline) (PublicationResult, error)
	Reconcile(context.Context, string, string) (PublicationResult, error)
}

// PublicationRetrier is the restricted failed-publication retry boundary.
type PublicationRetrier interface {
	Retry(context.Context, string, string) (PublicationResult, error)
}

// PublicationService owns final checks, Git publication, PR reconciliation,
// and the single durable done transition.
type PublicationService struct {
	reader      SpecificationManifestReader
	transition  SpecificationTransitioner
	reviews     ReviewEvidenceReader
	checks      checks.Runner
	worktree    gitrepo.WorktreeStateInspector
	git         gitrepo.PublicationRepository
	github      PullRequestGateway
	definitions []checks.Definition
}

func NewPublicationService(
	reader SpecificationManifestReader,
	transition SpecificationTransitioner,
	reviews ReviewEvidenceReader,
	checkRunner checks.Runner,
	worktree gitrepo.WorktreeStateInspector,
	git gitrepo.PublicationRepository,
	github PullRequestGateway,
	definitions []checks.Definition,
) *PublicationService {
	return &PublicationService{
		reader: reader, transition: transition, reviews: reviews, checks: checkRunner,
		worktree: worktree, git: git, github: github, definitions: append([]checks.Definition(nil), definitions...),
	}
}

// Finalize enters pull_request/running only for a durably approved review.
func (service *PublicationService) Finalize(ctx context.Context, controllerRoot, workflowID string, approvedState gitrepo.WorktreeBaseline) (PublicationResult, error) {
	if err := service.validate(); err != nil {
		return PublicationResult{}, err
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("read persisted workflow for publication: %w", err)
	}
	if current.Phase != state.PhaseReview || current.Status != state.StatusRunning || current.Review == nil || !hasRequirements(current) {
		return PublicationResult{}, fmt.Errorf("publication requires review/running state, got %s/%s", current.Phase, current.Status)
	}
	evidence, err := service.reviews.ReadReview(controllerRoot, workflowID)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("read current review approval: %w", err)
	}
	if !evidence.Approved {
		return PublicationResult{}, errors.New("publication requires current approved review evidence")
	}
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, current.Worktree)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("resolve approved publication worktree: %w", err)
	}
	if changed, err := service.worktree.Inspect(ctx, absoluteWorktree, approvedState); err != nil {
		return service.fail(controllerRoot, current, PublicationResult{Manifest: current}, fmt.Errorf("verify current approved worktree state: %w", err))
	} else if len(changed) > 0 {
		return service.fail(controllerRoot, current, PublicationResult{Manifest: current}, fmt.Errorf("worktree changed after approval: %s", strings.Join(changed, ", ")))
	}
	running := current
	running.Phase = state.PhasePullRequest
	running.Blocker = nil
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return PublicationResult{}, fmt.Errorf("persist pull_request/running transition: %w", err)
	}
	return service.publish(ctx, controllerRoot, running)
}

// Reconcile resumes an interrupted pull_request/running terminal phase.
func (service *PublicationService) Reconcile(ctx context.Context, controllerRoot, workflowID string) (PublicationResult, error) {
	if err := service.validate(); err != nil {
		return PublicationResult{}, err
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("read persisted workflow for publication reconciliation: %w", err)
	}
	if current.Phase != state.PhasePullRequest || current.Status != state.StatusRunning {
		return PublicationResult{}, fmt.Errorf("publication reconciliation requires pull_request/running state, got %s/%s", current.Phase, current.Status)
	}
	return service.publish(ctx, controllerRoot, current)
}

// Retry is intentionally restricted to a durable pull_request/failed state.
func (service *PublicationService) Retry(ctx context.Context, controllerRoot, workflowID string) (PublicationResult, error) {
	if err := service.validate(); err != nil {
		return PublicationResult{}, err
	}
	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("read persisted workflow for publication retry: %w", err)
	}
	if current.Phase != state.PhasePullRequest || current.Status != state.StatusFailed {
		return PublicationResult{}, fmt.Errorf("retry is supported only for pull_request/failed; got %s/%s; see the manual recovery guidance in docs/manual-recovery.md", current.Phase, current.Status)
	}
	running := current
	running.Status = state.StatusRunning
	running.LastError = nil
	if err := service.transition.Transition(controllerRoot, workflowID, running); err != nil {
		return PublicationResult{}, fmt.Errorf("persist pull_request retry transition: %w", err)
	}
	return service.publish(ctx, controllerRoot, running)
}

func (service *PublicationService) publish(ctx context.Context, controllerRoot string, running state.Manifest) (PublicationResult, error) {
	result := PublicationResult{Manifest: running}
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, running.Worktree)
	if err != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("resolve publication worktree: %w", err))
	}
	baseline, err := service.worktree.Capture(ctx, absoluteWorktree)
	if err != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("capture pre-check publication worktree: %w", err))
	}
	checkResults, err := service.checks.Run(ctx, absoluteWorktree, service.definitions)
	result.CheckResults = cloneCheckResults(checkResults)
	if err != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("run final deterministic checks: %w", err))
	}
	if changed, inspectErr := service.worktree.Inspect(ctx, absoluteWorktree, baseline); inspectErr != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("verify final checked worktree state: %w", inspectErr))
	} else if len(changed) > 0 {
		return service.fail(controllerRoot, running, result, fmt.Errorf("final deterministic checks changed worktree paths: %s", strings.Join(changed, ", ")))
	}
	if failed := failedCheckResults(checkResults); len(failed) > 0 {
		return service.fail(controllerRoot, running, result, errors.New("final deterministic checks failed"))
	}
	if !running.SkipSpecification {
		if err := state.VerifySpecificationFile(absoluteWorktree, running.SpecificationPath); err != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("verify generated specification before publication: %w", err))
		}
	}
	request := gitrepo.PublicationRequest{
		Worktree: absoluteWorktree, Branch: running.Branch, BaseSHA: running.BaseSHA,
		SkipSpecification: running.SkipSpecification, SpecificationPath: running.SpecificationPath, CommitMessage: fmt.Sprintf("awdev: implement issue #%d", running.Issue.Number),
		ExpectedTreeSHA: running.CommitTreeSHA, ExpectedCommitSHA: running.CommitSHA,
	}
	if running.CommitTreeSHA == "" {
		treeSHA, stageErr := service.git.Stage(ctx, request)
		if stageErr != nil {
			return service.fail(controllerRoot, running, result, stageErr)
		}
		withTree := running
		withTree.CommitTreeSHA = treeSHA
		if err := service.transition.Transition(controllerRoot, running.WorkflowID, withTree); err != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("persist controller commit tree intent: %w", err))
		}
		running = withTree
		result.Manifest = withTree
		request.ExpectedTreeSHA = treeSHA
	}
	commitSHA, err := service.git.Commit(ctx, request)
	if err != nil {
		return service.fail(controllerRoot, running, result, err)
	}
	if running.CommitSHA == "" {
		withCommit := running
		withCommit.CommitSHA = commitSHA
		if err := service.transition.Transition(controllerRoot, running.WorkflowID, withCommit); err != nil {
			return service.fail(controllerRoot, running, result, fmt.Errorf("persist controller commit identity: %w", err))
		}
		running = withCommit
		result.Manifest = withCommit
	} else if commitSHA != running.CommitSHA {
		return service.fail(controllerRoot, running, result, errors.New("Git returned a controller commit identity that does not match durable state"))
	}
	if err := service.git.Push(ctx, absoluteWorktree, running.Branch); err != nil {
		return service.fail(controllerRoot, running, result, err)
	}

	pullRequest, err := service.reconcilePullRequest(ctx, controllerRoot, running)
	if err != nil {
		return service.fail(controllerRoot, running, result, err)
	}
	withPullRequest := running
	withPullRequest.PullRequest = &state.PullRequest{Number: pullRequest.Number, URL: pullRequest.URL}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, withPullRequest); err != nil {
		return service.fail(controllerRoot, running, result, fmt.Errorf("persist pull request identity: %w", err))
	}
	result.Manifest = withPullRequest
	done := withPullRequest
	done.Phase = state.PhaseDone
	done.Status = state.StatusDone
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, done); err != nil {
		return service.fail(controllerRoot, withPullRequest, result, fmt.Errorf("persist terminal workflow state: %w", err))
	}
	result.Manifest = done
	return result, nil
}

func (service *PublicationService) reconcilePullRequest(ctx context.Context, controllerRoot string, manifest state.Manifest) (githubapi.PullRequest, error) {
	pullRequests, err := service.github.ListPullRequests(ctx, controllerRoot, manifest.Repository, manifest.Branch)
	if err != nil {
		return githubapi.PullRequest{}, err
	}
	if len(pullRequests) > 1 {
		return githubapi.PullRequest{}, errors.New("pull request lookup is ambiguous for the recorded branch")
	}
	if len(pullRequests) == 1 {
		if err := validatePullRequestIdentity(pullRequests[0], manifest); err != nil {
			return githubapi.PullRequest{}, err
		}
		if manifest.PullRequest != nil && (manifest.PullRequest.Number != pullRequests[0].Number || manifest.PullRequest.URL != pullRequests[0].URL) {
			return githubapi.PullRequest{}, errors.New("GitHub pull request does not match the identity already recorded by the workflow")
		}
		return pullRequests[0], nil
	}
	if manifest.PullRequest != nil {
		return githubapi.PullRequest{}, errors.New("the recorded pull request is missing from GitHub lookup; refusing to create a duplicate")
	}
	title := redactBlockerQuestion(manifest.Issue.Title)
	body := redactBlockerQuestion(fmt.Sprintf("Closes #%d\n\nThis pull request was created by awdev for GitHub issue #%d.", manifest.Issue.Number, manifest.Issue.Number))
	if title == "" || body == "" {
		return githubapi.PullRequest{}, errors.New("pull request text is empty after credential redaction")
	}
	created, err := service.github.CreatePullRequest(ctx, controllerRoot, githubapi.CreatePullRequestRequest{
		Repository: manifest.Repository, Head: manifest.Branch, Base: manifest.DefaultBranch, Title: title, Body: body,
	})
	if err != nil {
		return githubapi.PullRequest{}, err
	}
	if err := validatePullRequestIdentity(created, manifest); err != nil {
		return githubapi.PullRequest{}, err
	}
	return created, nil
}

func validatePullRequestIdentity(pullRequest githubapi.PullRequest, manifest state.Manifest) error {
	owner, _, _ := strings.Cut(manifest.Repository, "/")
	wantURL := fmt.Sprintf("https://github.com/%s/pull/%d", manifest.Repository, pullRequest.Number)
	if pullRequest.Number <= 0 || pullRequest.URL != wantURL || pullRequest.Head != manifest.Branch || pullRequest.HeadOwner != owner || pullRequest.Base != manifest.DefaultBranch {
		return errors.New("GitHub returned a pull request whose repository, head, or base does not match the workflow")
	}
	switch pullRequest.State {
	case "OPEN", "CLOSED", "MERGED":
		return nil
	default:
		return errors.New("GitHub returned a pull request with an invalid state")
	}
}

func (service *PublicationService) fail(controllerRoot string, running state.Manifest, result PublicationResult, cause error) (PublicationResult, error) {
	failed := running
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, running.WorkflowID, failed); err != nil {
		result.Manifest = running
		return result, &PublicationFailure{Cause: errors.Join(cause, fmt.Errorf("persist publication failure: %w", err)), CheckResults: cloneCheckResults(result.CheckResults)}
	}
	result.Manifest = failed
	return result, &PublicationFailure{Cause: cause, CheckResults: cloneCheckResults(result.CheckResults)}
}

func (service *PublicationService) validate() error {
	if service == nil || service.reader == nil || service.transition == nil || service.reviews == nil || service.checks == nil || service.worktree == nil || service.git == nil || service.github == nil {
		return errors.New("publication service is not fully configured")
	}
	return nil
}
