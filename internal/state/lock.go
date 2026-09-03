// Package state owns controller-local workflow state and synchronization.
package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const lockRetryInterval = 20 * time.Millisecond

// Lock releases one acquired workflow lock.
type Lock interface {
	Release() error
}

// Locker serializes operations for a deterministic workflow identity.
type Locker interface {
	Acquire(context.Context, string, string) (Lock, error)
}

// FileLocker uses an OS advisory lock. Stale lock-file contents are not used
// as evidence that a workflow is active; ownership belongs to the open file
// description and is released by the OS when its process exits.
type FileLocker struct{}

// NewFileLocker constructs a workflow file locker.
func NewFileLocker() FileLocker { return FileLocker{} }

// Acquire waits for the named workflow lock while honoring cancellation.
func (FileLocker) Acquire(ctx context.Context, controllerRoot, workflowID string) (Lock, error) {
	if err := validateGitHubIssueKey(workflowID); err != nil {
		return nil, err
	}
	stateDirectory, err := ensureLockDirectory(controllerRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare workflow lock directory: %w", err)
	}
	lockPath := filepath.Join(stateDirectory, workflowID+".lock")
	return acquirePlatformLock(ctx, lockPath)
}

func ensureLockDirectory(controllerRoot string) (string, error) {
	if !filepath.IsAbs(controllerRoot) || filepath.Clean(controllerRoot) != controllerRoot {
		return "", errors.New("controller root must be an absolute clean path")
	}
	components := []string{
		filepath.Join(controllerRoot, ".awdev"),
		filepath.Join(controllerRoot, ".awdev", "locks"),
	}
	for _, component := range components {
		info, err := os.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			mkdirErr := os.Mkdir(component, 0o755)
			info, err = os.Lstat(component)
			if err != nil && mkdirErr != nil {
				return "", mkdirErr
			}
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s must be a real directory, not a symlink", component)
		}
	}
	return components[len(components)-1], nil
}

func validateGitHubIssueKey(workflowID string) error {
	if !strings.HasPrefix(workflowID, "gh-") {
		return fmt.Errorf("invalid GitHub issue coordination key %q", workflowID)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(workflowID, "gh-"))
	want, wantErr := GitHubIssueKey(number)
	if err != nil || wantErr != nil || want != workflowID {
		return fmt.Errorf("invalid GitHub issue coordination key %q", workflowID)
	}
	return nil
}

func waitForLockRetry(ctx context.Context) error {
	timer := time.NewTimer(lockRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
