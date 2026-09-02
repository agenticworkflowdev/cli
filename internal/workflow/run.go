// Package workflow coordinates ordered application-level workflow operations.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"

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
	WorkflowID string
	Outcome    RunOutcome
	Snapshot   githubapi.Snapshot
	Worktree   gitrepo.Worktree
	Existing   state.ExistingWorkflow
}

// Bootstrap contains the validated transient data passed to Slice 3. It is not
// persisted by this service.
type Bootstrap struct {
	ControllerRoot string
	WorkflowID     string
	Snapshot       githubapi.Snapshot
}

// Bootstrapper continues the ordered run after GitHub validation.
type Bootstrapper interface {
	Continue(context.Context, Bootstrap) (gitrepo.Worktree, error)
}

// RunService downloads and validates a GitHub issue under its workflow lock.
type RunService struct {
	locker       state.Locker
	existing     state.ExistingReader
	github       githubapi.Fetcher
	bootstrapper Bootstrapper
}

// NewRunService constructs the run application service.
func NewRunService(locker state.Locker, existing state.ExistingReader, github githubapi.Fetcher, bootstrapper Bootstrapper) *RunService {
	return &RunService{locker: locker, existing: existing, github: github, bootstrapper: bootstrapper}
}

// RunGitHub validates one issue and holds the workflow lock through any
// installed next bootstrap step.
func (service *RunService) RunGitHub(ctx context.Context, controllerRoot string, issueNumber int) (result RunResult, err error) {
	if issueNumber <= 0 {
		return RunResult{}, errors.New("issue number must be positive")
	}
	if service.locker == nil || service.existing == nil || service.github == nil || service.bootstrapper == nil {
		return RunResult{}, errors.New("GitHub run service is not fully configured")
	}
	workflowID := "gh-" + strconv.Itoa(issueNumber)
	existing, err := service.existing.ReadExisting(controllerRoot, workflowID)
	if err != nil {
		return RunResult{}, fmt.Errorf("check existing workflow: %w", err)
	}
	if existing.Exists {
		return RunResult{WorkflowID: workflowID, Outcome: RunExisting, Existing: existing}, nil
	}

	lock, err := service.locker.Acquire(ctx, controllerRoot, workflowID)
	if err != nil {
		return RunResult{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = releaseErr
			result = RunResult{}
		}
	}()

	existing, err = service.existing.ReadExisting(controllerRoot, workflowID)
	if err != nil {
		return RunResult{}, fmt.Errorf("check existing workflow: %w", err)
	}
	if existing.Exists {
		return RunResult{WorkflowID: workflowID, Outcome: RunExisting, Existing: existing}, nil
	}

	snapshot, err := service.github.Fetch(ctx, controllerRoot, issueNumber)
	if err != nil {
		return RunResult{}, fmt.Errorf("download GitHub issue: %w", err)
	}
	if err := snapshot.Validate(issueNumber); err != nil {
		return RunResult{}, fmt.Errorf("validate GitHub issue: %w", err)
	}

	bootstrap := Bootstrap{ControllerRoot: controllerRoot, WorkflowID: workflowID, Snapshot: snapshot}
	worktree, err := service.bootstrapper.Continue(ctx, bootstrap)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{WorkflowID: workflowID, Outcome: RunReady, Snapshot: snapshot, Worktree: worktree}, nil
}
