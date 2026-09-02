package gitrepo_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
)

func TestDiscoverControllerRootFromRootAndNestedDirectory(t *testing.T) {
	repository := t.TempDir()
	command := exec.Command("git", "init", "--quiet", repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	nested := filepath.Join(repository, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}

	for _, directory := range []string{repository, nested} {
		t.Run(directory, func(t *testing.T) {
			root, err := gitrepo.DiscoverControllerRoot(context.Background(), directory)
			if err != nil {
				t.Fatalf("discover root: %v", err)
			}
			want, err := filepath.EvalSymlinks(repository)
			if err != nil {
				t.Fatalf("canonicalize repository: %v", err)
			}
			if root != want {
				t.Fatalf("root = %q, want %q", root, want)
			}
		})
	}
}

func TestDiscoverControllerRootRejectsNonRepository(t *testing.T) {
	_, err := gitrepo.DiscoverControllerRoot(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("discover root succeeded outside a Git repository")
	}
}

func TestDiscoverControllerRootUsesOriginalCheckoutFromLinkedWorktree(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "test@example.com")
	runGit(t, repository, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "--quiet", "-m", "initial")

	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, repository, "worktree", "add", "--quiet", "-b", "linked-test", worktree)

	root, err := gitrepo.DiscoverControllerRoot(context.Background(), worktree)
	if err != nil {
		t.Fatalf("discover root: %v", err)
	}
	want, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("canonicalize repository: %v", err)
	}
	if root != want {
		t.Fatalf("root = %q, want original checkout %q", root, want)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
