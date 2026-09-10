package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// ResumeOutcome distinguishes a safe wait from a newly continued workflow.
type ResumeOutcome string

const (
	ResumeWaiting   ResumeOutcome = "waiting"
	ResumeContinued ResumeOutcome = "continued"
)

// ResumeContinuation is returned by the phase-aware continuation boundary.
type ResumeContinuation struct {
	Manifest       state.Manifest
	Blocker        *BlockerRequest
	Specification  *SpecificationResult
	Implementation *ImplementationResult
	Review         *ReviewResult
}

// ResumeResult records answer selection and all phase work started by resume.
type ResumeResult struct {
	Outcome        ResumeOutcome
	Manifest       state.Manifest
	Answer         *state.BlockerAnswer
	Specification  *SpecificationResult
	Implementation *ImplementationResult
	Review         *ReviewResult
	Publication    *PublicationResult
}

// ResumePhaseContinuation continues the typed agent role for the phase that
// originally blocked, resuming its provider session when one was persisted.
type ResumePhaseContinuation interface {
	ContinueResume(context.Context, string, string, state.Phase) (ResumeContinuation, error)
}

// ResumeService selects one deterministic human answer while holding the
// workflow lock, durably resumes the blocker, and dispatches the recorded
// phase.
type ResumeService struct {
	locker       state.Locker
	existing     state.ExistingReader
	transition   SpecificationTransitioner
	comments     IssueCommentReader
	publisher    BlockerPublisher
	continuation ResumePhaseContinuation
	finalizer    Finalizer
}

// WithFinalizer installs terminal publication after a resumed review passes.
func (service *ResumeService) WithFinalizer(finalizer Finalizer) *ResumeService {
	service.finalizer = finalizer
	return service
}

// NewResumeService constructs GitHub resume orchestration.
func NewResumeService(locker state.Locker, existing state.ExistingReader, transition SpecificationTransitioner, comments IssueCommentReader, publisher BlockerPublisher, continuation ResumePhaseContinuation) *ResumeService {
	return &ResumeService{
		locker: locker, existing: existing, transition: transition, comments: comments,
		publisher: publisher, continuation: continuation,
	}
}

// ResumeGitHub resumes only a blocked workflow for the selected GitHub issue.
func (service *ResumeService) ResumeGitHub(ctx context.Context, controllerRoot string, issueNumber int) (result ResumeResult, err error) {
	if issueNumber <= 0 {
		return ResumeResult{}, errors.New("issue number must be positive")
	}
	if service == nil || service.locker == nil || service.existing == nil || service.transition == nil || service.comments == nil || service.continuation == nil {
		return ResumeResult{}, errors.New("GitHub resume service is not fully configured")
	}
	issueKey, err := state.GitHubIssueKey(issueNumber)
	if err != nil {
		return ResumeResult{}, err
	}
	lock, err := service.locker.Acquire(ctx, controllerRoot, issueKey)
	if err != nil {
		return ResumeResult{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = releaseErr
			result = ResumeResult{}
		}
	}()

	existing, err := service.existing.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("read workflow for resume: %w", err)
	}
	if !existing.Exists || existing.Manifest == nil {
		return ResumeResult{}, fmt.Errorf("no workflow exists for GitHub issue #%d", issueNumber)
	}
	current := *existing.Manifest
	if current.Source != state.SourceGitHub || current.Issue.Number != issueNumber {
		return ResumeResult{}, errors.New("saved workflow does not match the requested GitHub issue")
	}
	if current.Status != state.StatusBlocked || current.Blocker == nil || current.Blocker.Comment == nil {
		return ResumeResult{}, fmt.Errorf("resume requires a blocked workflow, got %s/%s", current.Phase, current.Status)
	}

	comments, err := service.comments.ListIssueComments(ctx, controllerRoot, current.Repository, issueNumber)
	if err != nil {
		return service.failBlocked(controllerRoot, current, fmt.Errorf("list GitHub replies: %w", err))
	}
	blockerComment, found := findRecordedBlockerComment(comments, current)
	if !found {
		return service.failBlocked(controllerRoot, current, errors.New("recorded blocker comment is missing from GitHub"))
	}
	answerComment, found := selectBlockerAnswer(comments, blockerComment, current.Repository, issueNumber)
	if !found {
		return ResumeResult{Outcome: ResumeWaiting, Manifest: current}, nil
	}

	answer := &state.BlockerAnswer{
		ID: answerComment.ID, URL: answerComment.URL, Body: answerComment.Body,
		Author: answerComment.Author.Login, CreatedAt: answerComment.CreatedAt,
	}
	resumed := current
	resumed.Status = state.StatusRunning
	resumed.Blocker = cloneBlocker(current.Blocker)
	resumed.Blocker.Answer = answer
	if err := service.transition.Transition(controllerRoot, current.WorkflowID, resumed); err != nil {
		return ResumeResult{Manifest: current}, fmt.Errorf("persist selected blocker answer: %w", err)
	}

	continued, err := service.continuation.ContinueResume(ctx, controllerRoot, current.WorkflowID, current.Phase)
	result = ResumeResult{
		Outcome: ResumeContinued, Manifest: continued.Manifest, Answer: answer,
		Specification: continued.Specification, Implementation: continued.Implementation, Review: continued.Review,
	}
	if err != nil {
		failed, failErr := service.ensureTechnicalFailure(controllerRoot, issueNumber, err)
		if failed.WorkflowID != "" {
			result.Manifest = failed
		}
		return result, failErr
	}
	if continued.Blocker != nil {
		if service.publisher == nil {
			return result, errors.New("blocker publisher is unavailable for repeated blocker")
		}
		published, publishErr := service.publisher.Publish(ctx, controllerRoot, current.WorkflowID, *continued.Blocker)
		result.Manifest = published
		return result, publishErr
	}
	if service.finalizer != nil {
		if result.Review == nil {
			return result, errors.New("resumed workflow reached publication without review evidence")
		}
		publication, publishErr := service.finalizer.Finalize(ctx, controllerRoot, current.WorkflowID, result.Review.CheckedState)
		if publication.Manifest.WorkflowID != "" {
			result.Manifest = publication.Manifest
		}
		result.Publication = &publication
		return result, publishErr
	}
	return result, nil
}

func (service *ResumeService) ensureTechnicalFailure(controllerRoot string, issueNumber int, cause error) (state.Manifest, error) {
	existing, err := service.existing.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return state.Manifest{}, errors.Join(cause, fmt.Errorf("read workflow after resume failure: %w", err))
	}
	if !existing.Exists || existing.Manifest == nil {
		return state.Manifest{}, errors.Join(cause, errors.New("workflow disappeared after resume failure"))
	}
	current := *existing.Manifest
	if current.Status == state.StatusFailed {
		return current, cause
	}
	if current.Status != state.StatusRunning {
		return current, errors.Join(cause, fmt.Errorf("cannot record resume failure from %s/%s", current.Phase, current.Status))
	}
	failed := current
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, current.WorkflowID, failed); err != nil {
		return current, errors.Join(cause, fmt.Errorf("persist continuation failure: %w", err))
	}
	return failed, cause
}

func findRecordedBlockerComment(comments []githubapi.IssueComment, manifest state.Manifest) (githubapi.IssueComment, bool) {
	for _, comment := range comments {
		if comment.ID == manifest.Blocker.Comment.ID && comment.URL == manifest.Blocker.Comment.URL &&
			hasCanonicalMarker(comment.Body, manifest.Blocker.Marker) && validIssueComment(comment, manifest.Repository, manifest.Issue.Number) {
			return comment, true
		}
	}
	return githubapi.IssueComment{}, false
}

func selectBlockerAnswer(comments []githubapi.IssueComment, blocker githubapi.IssueComment, repository string, issueNumber int) (githubapi.IssueComment, bool) {
	eligible := make([]githubapi.IssueComment, 0)
	for _, comment := range comments {
		if !validIssueComment(comment, repository, issueNumber) || strings.TrimSpace(comment.Body) == "" || comment.Author.Type != githubapi.CommentAuthorTypeUser ||
			strings.HasSuffix(strings.ToLower(comment.Author.Login), "[bot]") || compareCommentOrder(comment, blocker) <= 0 {
			continue
		}
		eligible = append(eligible, comment)
	}
	if len(eligible) == 0 {
		return githubapi.IssueComment{}, false
	}
	sort.SliceStable(eligible, func(left, right int) bool {
		return compareCommentOrder(eligible[left], eligible[right]) < 0
	})
	return eligible[0], true
}

func compareCommentOrder(left, right githubapi.IssueComment) int {
	if left.CreatedAt.Before(right.CreatedAt) {
		return -1
	}
	if left.CreatedAt.After(right.CreatedAt) {
		return 1
	}
	leftID, leftOK := new(big.Int).SetString(left.ID, 10)
	rightID, rightOK := new(big.Int).SetString(right.ID, 10)
	if leftOK && rightOK {
		return leftID.Cmp(rightID)
	}
	return strings.Compare(left.ID, right.ID)
}

func (service *ResumeService) failBlocked(controllerRoot string, current state.Manifest, cause error) (ResumeResult, error) {
	failed := current
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, current.WorkflowID, failed); err != nil {
		return ResumeResult{Manifest: current}, errors.Join(cause, fmt.Errorf("persist resume failure: %w", err))
	}
	return ResumeResult{Manifest: failed}, cause
}
