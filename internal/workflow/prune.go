package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
)

// WorkflowDeleter removes durable state for one workflow.
type WorkflowDeleter interface {
	Delete(string, string) error
}

// PruneReader loads workflow metadata even after partial Git cleanup.
type PruneReader interface {
	ReadExistingForPrune(string, int) (state.ExistingWorkflow, error)
}

// PruneResult identifies the workflow whose artifacts were removed.
type PruneResult struct {
	WorkflowID string
}

// PruneService removes all controller-owned artifacts for a workflow.
type PruneService struct {
	locker   state.Locker
	existing PruneReader
	git      gitrepo.WorktreePruner
	state    WorkflowDeleter
}

// NewPruneService constructs workflow cleanup orchestration.
func NewPruneService(locker state.Locker, existing PruneReader, git gitrepo.WorktreePruner, state WorkflowDeleter) *PruneService {
	return &PruneService{locker: locker, existing: existing, git: git, state: state}
}

// PruneGitHub removes the worktree, branch, and finally durable workflow state.
func (service *PruneService) PruneGitHub(ctx context.Context, controllerRoot string, issueNumber int) (result PruneResult, err error) {
	if issueNumber <= 0 {
		return PruneResult{}, errors.New("issue number must be positive")
	}
	if service == nil || service.locker == nil || service.existing == nil || service.git == nil || service.state == nil {
		return PruneResult{}, errors.New("prune service is not fully configured")
	}
	issueKey, err := state.GitHubIssueKey(issueNumber)
	if err != nil {
		return PruneResult{}, err
	}
	lock, err := service.locker.Acquire(ctx, controllerRoot, issueKey)
	if err != nil {
		return PruneResult{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = releaseErr
			result = PruneResult{}
		}
	}()

	existing, err := service.existing.ReadExistingForPrune(controllerRoot, issueNumber)
	if err != nil {
		return PruneResult{}, fmt.Errorf("read workflow for GitHub issue #%d: %w", issueNumber, err)
	}
	if !existing.Exists || existing.Manifest == nil {
		return PruneResult{}, fmt.Errorf("workflow for GitHub issue #%d does not exist", issueNumber)
	}
	manifest := *existing.Manifest
	absoluteWorktree, err := state.ResolveWorktreePath(controllerRoot, manifest.Worktree)
	if err != nil {
		return PruneResult{}, fmt.Errorf("resolve workflow worktree: %w", err)
	}
	if err := service.git.Prune(ctx, gitrepo.PruneRequest{
		ControllerRoot: controllerRoot,
		IssueNumber:    issueNumber,
		Branch:         manifest.Branch,
		WorktreePath:   absoluteWorktree,
	}); err != nil {
		return PruneResult{}, err
	}
	if err := service.state.Delete(controllerRoot, manifest.WorkflowID); err != nil {
		return PruneResult{}, fmt.Errorf("delete workflow state: %w", err)
	}
	return PruneResult{WorkflowID: manifest.WorkflowID}, nil
}
