package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestFileLockerSerializesSameWorkflowAndReleases(t *testing.T) {
	root := t.TempDir()
	locker := state.NewFileLocker()
	first, err := locker.Acquire(context.Background(), root, "gh-17")
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := locker.Acquire(ctx, root, "gh-17"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquisition error = %v, want deadline exceeded", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	second, err := locker.Acquire(context.Background(), root, "gh-17")
	if err != nil {
		t.Fatalf("reacquire released lock: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}

func TestFileLockerAllowsDifferentWorkflowsAndIgnoresStaleFile(t *testing.T) {
	root := t.TempDir()
	staleDirectory := filepath.Join(root, ".awdev", "issues", "gh-18")
	if err := os.MkdirAll(staleDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDirectory, ".lock"), []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	locker := state.NewFileLocker()
	first, err := locker.Acquire(context.Background(), root, "gh-17")
	if err != nil {
		t.Fatalf("acquire gh-17: %v", err)
	}
	defer first.Release()
	second, err := locker.Acquire(context.Background(), root, "gh-18")
	if err != nil {
		t.Fatalf("acquire different/stale workflow: %v", err)
	}
	defer second.Release()
}
