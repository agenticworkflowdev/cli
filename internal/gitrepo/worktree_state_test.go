package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestWorktreeInspectorDetectsTrackedAndUntrackedMutations(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, string)
		mutate func(*testing.T, string)
		want   []string
	}{
		{
			name: "unchanged",
			setup: func(t *testing.T, root string) {
				writeWorktreeFile(t, root, "notes.txt", "untracked\n", 0o644)
			},
			mutate: func(*testing.T, string) {},
			want:   []string{},
		},
		{
			name:   "tracked edit",
			mutate: func(t *testing.T, root string) { writeWorktreeFile(t, root, "tracked.txt", "changed\n", 0o644) },
			want:   []string{"tracked.txt"},
		},
		{
			name:   "untracked creation",
			mutate: func(t *testing.T, root string) { writeWorktreeFile(t, root, "new.txt", "new\n", 0o644) },
			want:   []string{"new.txt"},
		},
		{
			name: "empty directory creation",
			mutate: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"empty"},
		},
		{
			name: "untracked deletion",
			setup: func(t *testing.T, root string) {
				writeWorktreeFile(t, root, "notes.txt", "untracked\n", 0o644)
			},
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "notes.txt")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"notes.txt"},
		},
		{
			name: "mode change",
			mutate: func(t *testing.T, root string) {
				if runtime.GOOS == "windows" {
					t.Skip("mode bits are not portable on Windows")
				}
				if err := os.Chmod(filepath.Join(root, "tracked.txt"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"tracked.txt"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newSnapshotRepository(t)
			if test.setup != nil {
				test.setup(t, root)
			}
			inspector := gitrepo.NewWorktreeInspector("git", processrun.NewRunner())
			baseline, err := inspector.Capture(context.Background(), root)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			test.mutate(t, root)
			changed, err := inspector.Inspect(context.Background(), root, baseline)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if !reflect.DeepEqual(changed, test.want) {
				t.Fatalf("changed = %v, want %v", changed, test.want)
			}
		})
	}
}

func newSnapshotRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test User")
	writeWorktreeFile(t, root, "tracked.txt", "original\n", 0o644)
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "commit", "--quiet", "-m", "initial")
	return root
}

func writeWorktreeFile(t *testing.T, root, name, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
