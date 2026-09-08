// Package workflow coordinates ordered application-level workflow operations.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// RunOutcome distinguishes a freshly validated bootstrap from an idempotent
// return of durable workflow state.
type RunOutcome string

const (
	RunReady    RunOutcome = "ready"
	RunExisting RunOutcome = "existing"
)

// RunResult is rendered by the CLI and carries typed bootstrap data forward.
type RunResult struct {
	WorkflowID     string
	Outcome        RunOutcome
	Snapshot       githubapi.Snapshot
	Worktree       gitrepo.Worktree
	Existing       state.ExistingWorkflow
	Manifest       *state.Manifest
	Specification  *SpecificationResult
	Implementation *ImplementationResult
	Review         *ReviewResult
	Publication    *PublicationResult
}

// Bootstrap contains the validated transient data passed to Slice 3. It is not
// persisted by this service.
type Bootstrap struct {
	ControllerRoot string
	Snapshot       githubapi.Snapshot
}

// Bootstrapper continues the ordered run after GitHub validation.
type Bootstrapper interface {
	Continue(context.Context, Bootstrap) (gitrepo.Worktree, error)
}

// RunService downloads and validates a GitHub issue under its workflow lock.
type RunService struct {
	locker         state.Locker
	existing       state.ExistingReader
	github         githubapi.Fetcher
	bootstrapper   Bootstrapper
	manifestWriter state.ManifestWriter
	workflowIDs    func() (string, error)
	specification  SpecificationCreator
	implementation ImplementationRunner
	reviewer       Reviewer
	publisher      BlockerPublisher
	finalizer      Finalizer
}

// WithFinalizer installs the terminal publication phase.
func (service *RunService) WithFinalizer(finalizer Finalizer) *RunService {
	service.finalizer = finalizer
	return service
}

// NewRunService constructs the run application service.
func NewRunService(locker state.Locker, existing state.ExistingReader, github githubapi.Fetcher, bootstrapper Bootstrapper, manifestWriter state.ManifestWriter, workflowIDs func() (string, error), specification SpecificationCreator, implementation ImplementationRunner, reviewer Reviewer, publisher ...BlockerPublisher) *RunService {
	service := &RunService{
		locker: locker, existing: existing, github: github, bootstrapper: bootstrapper,
		manifestWriter: manifestWriter, workflowIDs: workflowIDs, specification: specification, implementation: implementation, reviewer: reviewer,
	}
	if len(publisher) > 0 {
		service.publisher = publisher[0]
	}
	return service
}

// RunGitHub validates one issue and holds the workflow lock through any
// installed next bootstrap step.
func (service *RunService) RunGitHub(ctx context.Context, controllerRoot string, issueNumber int) (result RunResult, err error) {
	if issueNumber <= 0 {
		return RunResult{}, errors.New("issue number must be positive")
	}
	if service.locker == nil || service.existing == nil || service.github == nil || service.bootstrapper == nil || service.manifestWriter == nil || service.workflowIDs == nil || service.specification == nil || service.implementation == nil || service.reviewer == nil {
		return RunResult{}, errors.New("GitHub run service is not fully configured")
	}
	issueKey, err := state.GitHubIssueKey(issueNumber)
	if err != nil {
		return RunResult{}, err
	}
	existing, err := service.existing.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return RunResult{}, fmt.Errorf("check existing workflow: %w", err)
	}
	if existing.Exists && !pendingBlockerIntent(existing.Manifest) && !pendingPublication(existing.Manifest, service.finalizer) {
		return RunResult{WorkflowID: existing.Manifest.WorkflowID, Outcome: RunExisting, Existing: existing}, nil
	}

	lock, err := service.locker.Acquire(ctx, controllerRoot, issueKey)
	if err != nil {
		return RunResult{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = releaseErr
			result = RunResult{}
		}
	}()

	existing, err = service.existing.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return RunResult{}, fmt.Errorf("check existing workflow: %w", err)
	}
	if existing.Exists {
		if pendingBlockerIntent(existing.Manifest) {
			if service.publisher == nil {
				return RunResult{}, errors.New("blocker publisher is unavailable")
			}
			published, publishErr := service.publisher.Publish(ctx, controllerRoot, existing.Manifest.WorkflowID, BlockerRequest{Phase: existing.Manifest.Blocker.Phase, Question: existing.Manifest.Blocker.Question})
			existing.Manifest = &published
			return RunResult{WorkflowID: published.WorkflowID, Outcome: RunExisting, Existing: existing, Manifest: &published}, publishErr
		}
		if pendingPublication(existing.Manifest, service.finalizer) {
			publication, publishErr := service.finalizer.Reconcile(ctx, controllerRoot, existing.Manifest.WorkflowID)
			if publication.Manifest.WorkflowID != "" {
				existing.Manifest = &publication.Manifest
			}
			return RunResult{WorkflowID: existing.Manifest.WorkflowID, Outcome: RunExisting, Existing: existing, Manifest: existing.Manifest, Publication: &publication}, publishErr
		}
		return RunResult{WorkflowID: existing.Manifest.WorkflowID, Outcome: RunExisting, Existing: existing}, nil
	}

	snapshot, err := service.github.Fetch(ctx, controllerRoot, issueNumber)
	if err != nil {
		return RunResult{}, fmt.Errorf("download GitHub issue: %w", err)
	}
	if err := snapshot.Validate(issueNumber); err != nil {
		return RunResult{}, fmt.Errorf("validate GitHub issue: %w", err)
	}

	workflowID, err := service.workflowIDs()
	if err != nil {
		return RunResult{}, err
	}
	bootstrap := Bootstrap{ControllerRoot: controllerRoot, Snapshot: snapshot}
	worktree, err := service.bootstrapper.Continue(ctx, bootstrap)
	if err != nil {
		return RunResult{}, err
	}
	relativeWorktree, err := filepath.Rel(controllerRoot, worktree.AbsolutePath)
	if err != nil {
		return RunResult{}, fmt.Errorf("derive repository-relative worktree path: %w", err)
	}
	relativeWorktree = filepath.ToSlash(relativeWorktree)
	manifest := state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    workflowID,
		Source:        state.SourceGitHub,
		Repository:    snapshot.Repository.NameWithOwner,
		DefaultBranch: snapshot.Repository.DefaultBranch,
		Issue: state.IssueSnapshot{
			Number:    snapshot.Issue.Number,
			Title:     snapshot.Issue.Title,
			Body:      snapshot.Issue.Body,
			URL:       snapshot.Issue.URL,
			State:     snapshot.Issue.State,
			UpdatedAt: snapshot.Issue.UpdatedAt,
		},
		Actor:    snapshot.Actor.Login,
		Phase:    state.PhaseInit,
		Status:   state.StatusRunning,
		Branch:   worktree.Branch,
		BaseSHA:  worktree.BaseSHA,
		Worktree: relativeWorktree,
	}
	if err := service.manifestWriter.Save(controllerRoot, manifest); err != nil {
		return RunResult{}, fmt.Errorf("persist initial workflow manifest: %w", err)
	}
	specification, err := service.specification.Create(ctx, controllerRoot, workflowID)
	if err != nil {
		return RunResult{}, err
	}
	if specification.Blocker != nil {
		published, publishErr := service.publishBlocker(ctx, controllerRoot, workflowID, specification.Blocker)
		specification.Manifest = published
		return RunResult{WorkflowID: workflowID, Outcome: RunReady, Snapshot: snapshot, Worktree: worktree, Manifest: &specification.Manifest, Specification: &specification}, publishErr
	}
	implementation, err := service.implementation.Implement(ctx, controllerRoot, workflowID)
	if err != nil {
		return RunResult{
			WorkflowID: workflowID, Outcome: RunReady, Snapshot: snapshot, Worktree: worktree,
			Manifest: &implementation.Manifest, Specification: &specification, Implementation: &implementation,
		}, err
	}
	if implementation.Blocker != nil {
		published, publishErr := service.publishBlocker(ctx, controllerRoot, workflowID, implementation.Blocker)
		implementation.Manifest = published
		return RunResult{
			WorkflowID: workflowID, Outcome: RunReady, Snapshot: snapshot, Worktree: worktree,
			Manifest: &implementation.Manifest, Specification: &specification, Implementation: &implementation,
		}, publishErr
	}
	reviewResult, err := service.reviewer.Review(ctx, controllerRoot, workflowID, implementation.CheckResults, implementation.CheckedState)
	if err == nil && reviewResult.Blocker != nil {
		published, publishErr := service.publishBlocker(ctx, controllerRoot, workflowID, reviewResult.Blocker)
		reviewResult.Manifest = published
		err = publishErr
	}
	result = RunResult{
		WorkflowID: workflowID, Outcome: RunReady, Snapshot: snapshot, Worktree: worktree,
		Manifest: &reviewResult.Manifest, Specification: &specification, Implementation: &implementation, Review: &reviewResult,
	}
	if err != nil || reviewResult.Blocker != nil || service.finalizer == nil {
		return result, err
	}
	publication, err := service.finalizer.Finalize(ctx, controllerRoot, workflowID, reviewResult.CheckedState)
	if publication.Manifest.WorkflowID != "" {
		result.Manifest = &publication.Manifest
	}
	result.Publication = &publication
	return result, err
}

func pendingBlockerIntent(manifest *state.Manifest) bool {
	return manifest != nil && manifest.Status == state.StatusRunning && manifest.Blocker != nil && manifest.Blocker.Answer == nil
}

func pendingPublication(manifest *state.Manifest, finalizer Finalizer) bool {
	return finalizer != nil && manifest != nil && manifest.Phase == state.PhasePullRequest && manifest.Status == state.StatusRunning
}

func (service *RunService) publishBlocker(ctx context.Context, controllerRoot, workflowID string, blocker *BlockerRequest) (state.Manifest, error) {
	if service.publisher == nil {
		return state.Manifest{}, errors.New("blocker publisher is unavailable")
	}
	return service.publisher.Publish(ctx, controllerRoot, workflowID, *blocker)
}
