package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestDiffInspectorRejectsGitControllerAndConfiguredProtectedPaths(t *testing.T) {
	controllerRoot := t.TempDir()
	worktree := filepath.Join(controllerRoot, ".awdev", "worktrees", "gh-7-checks")
	if err := os.MkdirAll(filepath.Join(controllerRoot, ".awdev", "issues"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMarker := filepath.Join(worktree, ".git")
	if err := os.WriteFile(gitMarker, []byte("gitdir: original"), 0o644); err != nil {
		t.Fatal(err)
	}
	process := &diffProcessRunner{result: processrun.Result{Stdout: []byte(" M allowed.go\x00?? generated-sibling/file.txt\x00 M .gitmodules\x00")}}
	inspector := gitrepo.NewDiffInspector("git", process)
	scope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree, ProtectedPrefixes: []string{"generated"}}
	baseline, err := inspector.Capture(scope)
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}

	if err := os.WriteFile(gitMarker, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	controllerFile := filepath.Join(controllerRoot, ".awdev", "issues", "wf_test", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(controllerFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controllerFile, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	protectedFile := filepath.Join(worktree, "generated", "file.txt")
	if err := os.MkdirAll(filepath.Dir(protectedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protectedFile, []byte("ignored protected change"), 0o644); err != nil {
		t.Fatal(err)
	}

	offending, err := inspector.Inspect(context.Background(), scope, baseline)
	if err != nil {
		t.Fatalf("inspect diff: %v", err)
	}
	want := []string{".awdev/issues/wf_test/manifest.json", ".git", ".gitmodules", "generated/file.txt"}
	if !reflect.DeepEqual(offending, want) {
		t.Fatalf("offending paths = %#v, want %#v", offending, want)
	}
	if len(process.requests) != 1 || process.requests[0].Directory != worktree {
		t.Fatalf("git requests = %#v", process.requests)
	}
	wantArgv := []string{"git", "status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if !reflect.DeepEqual(process.requests[0].Argv, wantArgv) {
		t.Fatalf("git argv = %#v, want %#v", process.requests[0].Argv, wantArgv)
	}
}

func TestDiffInspectorAcceptsCleanAllowedDiffAndHandlesRenamePaths(t *testing.T) {
	controllerRoot := t.TempDir()
	worktree := filepath.Join(controllerRoot, ".awdev", "worktrees", "gh-7-checks")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	process := &diffProcessRunner{result: processrun.Result{Stdout: []byte("R  docs/new.md\x00generated/old.md\x00?? source.go\x00")}}
	inspector := gitrepo.NewDiffInspector("git", process)
	scope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree, ProtectedPrefixes: []string{"generated-assets"}}
	baseline, err := inspector.Capture(scope)
	if err != nil {
		t.Fatal(err)
	}

	offending, err := inspector.Inspect(context.Background(), scope, baseline)
	if err != nil {
		t.Fatalf("inspect diff: %v", err)
	}
	if len(offending) != 0 {
		t.Fatalf("allowed diff was rejected: %#v", offending)
	}

	process.result.Stdout = []byte("R  allowed/new.md\x00generated-assets/old.md\x00")
	offending, err = inspector.Inspect(context.Background(), scope, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offending, []string{"generated-assets/old.md"}) {
		t.Fatalf("renamed protected source = %#v", offending)
	}
}

func TestDiffInspectorRejectsLinkedGitDirectoryMutationWithCleanStatus(t *testing.T) {
	controllerRoot := t.TempDir()
	worktree := filepath.Join(controllerRoot, ".awdev", "worktrees", "gh-7-checks")
	gitDirectory := filepath.Join(controllerRoot, ".git", "worktrees", "gh-7-checks")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDirectory+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(gitDirectory, "index")
	if err := os.WriteFile(indexPath, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDirectory, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commonConfig := filepath.Join(controllerRoot, ".git", "config")
	if err := os.WriteFile(commonConfig, []byte("before config"), 0o644); err != nil {
		t.Fatal(err)
	}
	process := &diffProcessRunner{result: processrun.Result{Stdout: nil}}
	inspector := gitrepo.NewDiffInspector("git", process)
	scope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree}
	baseline, err := inspector.Capture(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("after staging or commit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commonConfig, []byte("after config"), 0o644); err != nil {
		t.Fatal(err)
	}

	offending, err := inspector.Inspect(context.Background(), scope, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offending, []string{".git/config", ".git/index"}) {
		t.Fatalf("offending Git metadata = %#v", offending)
	}
}

func TestDiffInspectorIgnoresVolatileFetchHeadChanges(t *testing.T) {
	controllerRoot := t.TempDir()
	worktree := filepath.Join(controllerRoot, ".awdev", "worktrees", "gh-7-checks")
	gitDirectory := filepath.Join(controllerRoot, ".git", "worktrees", "gh-7-checks")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDirectory+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDirectory, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(controllerRoot, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("before fetch"), 0o644); err != nil {
		t.Fatal(err)
	}

	inspector := gitrepo.NewDiffInspector("git", &diffProcessRunner{result: processrun.Result{Stdout: nil}})
	scope := gitrepo.DiffScope{ControllerRoot: controllerRoot, Worktree: worktree}
	baseline, err := inspector.Capture(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fetchHead, []byte("after auto-fetch"), 0o644); err != nil {
		t.Fatal(err)
	}

	offending, err := inspector.Inspect(context.Background(), scope, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(offending) != 0 {
		t.Fatalf("volatile fetch state was rejected: %#v", offending)
	}
}

type diffProcessRunner struct {
	result   processrun.Result
	err      error
	requests []processrun.Request
}

func (runner *diffProcessRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	runner.requests = append(runner.requests, request)
	return runner.result, runner.err
}
