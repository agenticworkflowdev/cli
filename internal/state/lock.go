// Package state owns controller-local workflow state and synchronization.
package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	if err := validateWorkflowID(workflowID); err != nil {
		return nil, err
	}
	stateDirectory := filepath.Join(controllerRoot, ".awdev", "issues", workflowID)
	if err := os.MkdirAll(stateDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("prepare workflow state directory: %w", err)
	}
	lockPath := filepath.Join(stateDirectory, ".lock")
	return acquirePlatformLock(ctx, lockPath)
}

func validateWorkflowID(workflowID string) error {
	if len(workflowID) < 4 || workflowID[:3] != "gh-" {
		return fmt.Errorf("invalid workflow identity %q", workflowID)
	}
	for _, character := range workflowID[3:] {
		if character < '0' || character > '9' {
			return fmt.Errorf("invalid workflow identity %q", workflowID)
		}
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
