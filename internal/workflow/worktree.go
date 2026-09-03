package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
)

type worktreeBootstrapper struct {
	preparer gitrepo.WorktreePreparer
}

// NewWorktreeBootstrapper adapts repository worktree operations to the ordered
// workflow bootstrap boundary.
func NewWorktreeBootstrapper(preparer gitrepo.WorktreePreparer) Bootstrapper {
	return &worktreeBootstrapper{preparer: preparer}
}

func (bootstrapper *worktreeBootstrapper) Continue(ctx context.Context, bootstrap Bootstrap) (gitrepo.Worktree, error) {
	if bootstrapper.preparer == nil {
		return gitrepo.Worktree{}, errors.New("worktree bootstrapper is not configured")
	}
	worktree, err := bootstrapper.preparer.Prepare(ctx, gitrepo.WorktreeRequest{
		ControllerRoot:     bootstrap.ControllerRoot,
		RepositoryIdentity: bootstrap.Snapshot.Repository.NameWithOwner,
		DefaultBranch:      bootstrap.Snapshot.Repository.DefaultBranch,
		IssueNumber:        bootstrap.Snapshot.Issue.Number,
		IssueTitle:         bootstrap.Snapshot.Issue.Title,
	})
	if err != nil {
		return gitrepo.Worktree{}, fmt.Errorf("prepare issue worktree: %w", err)
	}
	return worktree, nil
}
