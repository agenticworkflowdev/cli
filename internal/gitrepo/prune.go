package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PruneRequest identifies controller-owned Git artifacts for one workflow.
type PruneRequest struct {
	ControllerRoot string
	IssueNumber    int
	Branch         string
	WorktreePath   string
}

// WorktreePruner removes workflow worktrees and branches.
type WorktreePruner interface {
	Prune(context.Context, PruneRequest) error
}

// Prune removes a workflow worktree before deleting its branch. Missing
// artifacts are accepted so an interrupted prune can be retried safely.
func (manager *WorktreeManager) Prune(ctx context.Context, request PruneRequest) error {
	if manager == nil || manager.runner == nil || strings.TrimSpace(manager.binary) == "" {
		return errors.New("Git worktree manager is not fully configured")
	}
	if request.IssueNumber <= 0 {
		return errors.New("issue number must be positive")
	}
	controllerRoot, err := canonicalExistingDirectory(request.ControllerRoot)
	if err != nil {
		return fmt.Errorf("validate controller root: %w", err)
	}
	if !filepath.IsAbs(request.WorktreePath) || filepath.Clean(request.WorktreePath) != request.WorktreePath {
		return errors.New("workflow worktree path must be absolute and clean")
	}
	worktreeArea := filepath.Join(controllerRoot, ".awdev", "worktrees")
	artifactName := filepath.Base(request.WorktreePath)
	expectedPrefix := "gh-" + strconv.Itoa(request.IssueNumber) + "-"
	if !samePath(filepath.Dir(request.WorktreePath), worktreeArea) || request.Branch != artifactName || !strings.HasPrefix(request.Branch, expectedPrefix) || !validRefComponentPath(request.Branch) {
		return errors.New("workflow Git artifacts do not match the requested GitHub issue")
	}

	entries, err := manager.listWorktrees(ctx, controllerRoot)
	if err != nil {
		return err
	}
	registered := false
	for _, entry := range entries {
		if samePath(entry.path, request.WorktreePath) {
			if entry.branch != "" && entry.branch != request.Branch {
				return fmt.Errorf("workflow worktree has unexpected branch %q; preserving it for diagnosis", entry.branch)
			}
			registered = true
			continue
		}
		if entry.branch == request.Branch {
			return fmt.Errorf("workflow branch %q is checked out in another worktree; preserving it for diagnosis", request.Branch)
		}
	}
	if registered {
		if err := manager.command(ctx, controllerRoot, "worktree", "remove", "--force", request.WorktreePath); err != nil {
			return fmt.Errorf("remove workflow worktree: %w", err)
		}
	} else if _, err := os.Lstat(request.WorktreePath); err == nil {
		return fmt.Errorf("workflow worktree path %q exists but is not registered; preserving it for diagnosis", request.WorktreePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workflow worktree path: %w", err)
	}

	exists, err := manager.branchExists(ctx, controllerRoot, request.Branch)
	if err != nil {
		return err
	}
	if exists {
		if err := manager.command(ctx, controllerRoot, "branch", "-D", request.Branch); err != nil {
			return fmt.Errorf("delete workflow branch: %w", err)
		}
	}
	return nil
}
