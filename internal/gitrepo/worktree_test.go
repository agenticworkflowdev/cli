package gitrepo_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestIssueSlugNormalizesUntrustedTitles(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "ascii", title: "Create and Validate the Worktree", want: "create-and-validate-the-worktree"},
		{name: "unicode only", title: "日本語 🚀", want: "issue"},
		{name: "punctuation", title: "...Hello, WORLD!!!", want: "hello-world"},
		{name: "repeated separators", title: "one / \\ two___three", want: "one-two-three"},
		{name: "path like", title: "../../.git/hooks/pre-commit", want: "git-hooks-pre-commit"},
		{name: "option and shell syntax", title: "--upload-pack=$(touch nope); `whoami`", want: "upload-pack-touch-nope-whoami"},
		{name: "collision relevant first spelling", title: "A/B", want: "a-b"},
		{name: "collision relevant second spelling", title: "A B", want: "a-b"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gitrepo.IssueSlug(test.title); got != test.want {
				t.Fatalf("IssueSlug(%q) = %q, want %q", test.title, got, test.want)
			}
		})
	}
}

func TestIssueSlugHasFixedSafeMaximum(t *testing.T) {
	slug := gitrepo.IssueSlug(strings.Repeat("abcd-", 50))
	if len(slug) == 0 || len(slug) > gitrepo.MaxIssueSlugLength {
		t.Fatalf("slug length = %d, want 1..%d", len(slug), gitrepo.MaxIssueSlugLength)
	}
	if strings.HasSuffix(slug, "-") {
		t.Fatalf("slug %q ends with a separator", slug)
	}
	if slug != gitrepo.IssueSlug(strings.Repeat("abcd-", 50)) {
		t.Fatal("slug is not deterministic")
	}
}

func TestWorktreeManagerCreatesAndExactlyReentersPinnedWorktree(t *testing.T) {
	controller, baseSHA := newRemoteRepository(t)
	request := gitrepo.WorktreeRequest{
		ControllerRoot:     controller,
		RepositoryIdentity: "owner/repository",
		DefaultBranch:      "main",
		IssueNumber:        17,
		IssueTitle:         "Create ../safe worktree; $(touch nope)",
	}
	runner := &recordingGitRunner{delegate: processrun.NewRunner()}
	manager := gitrepo.NewWorktreeManager("git", runner)

	created, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}
	canonicalController, err := filepath.EvalSymlinks(controller)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(canonicalController, ".awdev", "worktrees", "gh-17-create-safe-worktree-touch-nope")
	if created.Branch != "gh-17-create-safe-worktree-touch-nope" || created.BaseSHA != baseSHA || created.AbsolutePath != wantPath {
		t.Fatalf("created worktree = %#v", created)
	}
	if got := gitOutput(t, controller, "branch", "--show-current"); got != "main" {
		t.Fatalf("controller branch = %q, want main", got)
	}
	if got := gitOutput(t, wantPath, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("worktree HEAD = %q, want %q", got, baseSHA)
	}
	if got := gitOutput(t, controller, "status", "--porcelain"); got != "" {
		t.Fatalf("controller checkout was modified: %q", got)
	}
	assertSafeGitRequests(t, runner.requests, request.IssueTitle, created)

	runner.requests = nil
	reentered, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("reenter exact worktree: %v", err)
	}
	if reentered != created {
		t.Fatalf("reentered worktree = %#v, want %#v", reentered, created)
	}
	for _, request := range runner.requests {
		if containsSequence(request.Argv, "worktree", "add") {
			t.Fatalf("exact re-entry attempted another worktree creation: %#v", request.Argv)
		}
	}
	if _, err := os.Stat(filepath.Join(controller, "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("title shell syntax was executed: %v", err)
	}
}

func TestWorktreeManagerReportsStaleRegistrationWithMissingFolder(t *testing.T) {
	controller, _ := newRemoteRepository(t)
	created, err := prepareTitleWorktree(controller)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.RemoveAll(created.AbsolutePath); err != nil {
		t.Fatalf("remove worktree folder: %v", err)
	}

	_, err = prepareTitleWorktree(controller)
	want := "deterministic worktree state is incomplete:\n- folder: missing (" + created.AbsolutePath + ")\n- worktree: stale\n- branch: exists (" + created.Branch + ")"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want status block %q", err, want)
	}
}

func TestWorktreeManagerRejectsShallowRepositoryBeforeCreation(t *testing.T) {
	root := t.TempDir()
	runner := &scriptedGitRunner{stdout: []string{
		"https://github.com/owner/repository.git\n",
		"true\n",
	}}
	manager := gitrepo.NewWorktreeManager("git", runner)
	_, err := manager.Prepare(context.Background(), gitrepo.WorktreeRequest{
		ControllerRoot:     root,
		RepositoryIdentity: "owner/repository",
		DefaultBranch:      "main",
		IssueNumber:        17,
		IssueTitle:         "title",
	})
	if err == nil || !strings.Contains(err.Error(), "shallow controller repositories are unsupported") {
		t.Fatalf("error = %v, want precise shallow-repository failure", err)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("Git calls = %d, want identity and shallow checks only", len(runner.requests))
	}
	if _, err := os.Stat(filepath.Join(root, ".awdev")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shallow failure created artifacts: %v", err)
	}
}

func TestWorktreeManagerRejectsSymlinkedControllerWorktreeArea(t *testing.T) {
	controller, _ := newRemoteRepository(t)
	worktreeArea := filepath.Join(controller, ".awdev", "worktrees")
	if err := os.Remove(worktreeArea); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, worktreeArea); err != nil {
		t.Fatal(err)
	}

	_, err := prepareTitleWorktree(controller)
	if err == nil || !strings.Contains(err.Error(), "controller-owned worktree area") {
		t.Fatalf("error = %v, want symlinked-area rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "gh-17-title")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree escaped through symlink: %v", statErr)
	}
}

func TestWorktreeManagerRejectsAndPreservesBootstrapCollisions(t *testing.T) {
	t.Run("branch without worktree", func(t *testing.T) {
		controller, baseSHA := newRemoteRepository(t)
		runGitCommand(t, controller, "branch", "gh-17-title", baseSHA)

		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "already exists without its exact worktree") {
			t.Fatalf("error = %v, want standalone branch collision", err)
		}
		if got := gitOutput(t, controller, "rev-parse", "gh-17-title"); got != baseSHA {
			t.Fatalf("colliding branch changed to %q", got)
		}
	})

	t.Run("unregistered directory", func(t *testing.T) {
		controller, _ := newRemoteRepository(t)
		path := filepath.Join(controller, ".awdev", "worktrees", "gh-17-title")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "keep-me")
		if err := os.WriteFile(marker, []byte("diagnostic"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "exists but is not registered") {
			t.Fatalf("error = %v, want unregistered-directory collision", err)
		}
		if contents, readErr := os.ReadFile(marker); readErr != nil || string(contents) != "diagnostic" {
			t.Fatalf("collision was not preserved: contents=%q err=%v", contents, readErr)
		}
	})

	t.Run("branch at another path", func(t *testing.T) {
		controller, baseSHA := newRemoteRepository(t)
		otherPath := filepath.Join(t.TempDir(), "other-worktree")
		runGitCommand(t, controller, "worktree", "add", "--quiet", "-b", "gh-17-title", otherPath, baseSHA)

		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "registered at conflicting path") {
			t.Fatalf("error = %v, want branch/path collision", err)
		}
		if got := gitOutput(t, otherPath, "branch", "--show-current"); got != "gh-17-title" {
			t.Fatalf("colliding branch changed to %q", got)
		}
	})

	t.Run("path on another branch", func(t *testing.T) {
		controller, baseSHA := newRemoteRepository(t)
		path := filepath.Join(controller, ".awdev", "worktrees", "gh-17-title")
		runGitCommand(t, controller, "worktree", "add", "--quiet", "-b", "unrelated", path, baseSHA)

		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "registered on conflicting branch") {
			t.Fatalf("error = %v, want path/branch collision", err)
		}
		if got := gitOutput(t, path, "branch", "--show-current"); got != "unrelated" {
			t.Fatalf("colliding path changed to branch %q", got)
		}
	})

	t.Run("wrong HEAD", func(t *testing.T) {
		controller, oldSHA := newRemoteRepository(t)
		created, err := prepareTitleWorktree(controller)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(controller, "README.md"), []byte("new base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitCommand(t, controller, "add", "README.md")
		runGitCommand(t, controller, "commit", "--quiet", "-m", "advance base")
		runGitCommand(t, controller, "push", "--quiet", "origin", "main")

		_, err = prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "does not match pinned base") {
			t.Fatalf("error = %v, want HEAD collision", err)
		}
		if got := gitOutput(t, created.AbsolutePath, "rev-parse", "HEAD"); got != oldSHA {
			t.Fatalf("collision HEAD changed to %q, want preserved %q", got, oldSHA)
		}
	})

	t.Run("different repository", func(t *testing.T) {
		controller, _ := newRemoteRepository(t)
		created, err := prepareTitleWorktree(controller)
		if err != nil {
			t.Fatal(err)
		}
		other := t.TempDir()
		runGitCommand(t, "", "init", "--quiet", other)
		maliciousGitfile := []byte("gitdir: " + filepath.Join(other, ".git") + "\n")
		gitfile := filepath.Join(created.AbsolutePath, ".git")
		if err := os.WriteFile(gitfile, maliciousGitfile, 0o644); err != nil {
			t.Fatal(err)
		}

		_, err = prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "different repository") {
			t.Fatalf("error = %v, want repository-identity collision", err)
		}
		if contents, readErr := os.ReadFile(gitfile); readErr != nil || !reflect.DeepEqual(contents, maliciousGitfile) {
			t.Fatalf("repository collision was modified: contents=%q err=%v", contents, readErr)
		}
	})
}

func TestWorktreeManagerRejectsWrongOrMissingOrigin(t *testing.T) {
	t.Run("wrong identity", func(t *testing.T) {
		controller, _ := newRemoteRepository(t)
		runGitCommand(t, controller, "remote", "set-url", "origin", "git@github.com:other/repository.git")
		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "does not match GitHub repository") {
			t.Fatalf("error = %v, want origin identity mismatch", err)
		}
	})

	t.Run("missing origin", func(t *testing.T) {
		controller, _ := newRemoteRepository(t)
		runGitCommand(t, controller, "remote", "remove", "origin")
		_, err := prepareTitleWorktree(controller)
		if err == nil || !strings.Contains(err.Error(), "required origin") {
			t.Fatalf("error = %v, want missing origin", err)
		}
	})
}

func prepareTitleWorktree(controller string) (gitrepo.Worktree, error) {
	return gitrepo.NewWorktreeManager("git", processrun.NewRunner()).Prepare(context.Background(), gitrepo.WorktreeRequest{
		ControllerRoot:     controller,
		RepositoryIdentity: "owner/repository",
		DefaultBranch:      "main",
		IssueNumber:        17,
		IssueTitle:         "title",
	})
}

func newRemoteRepository(t *testing.T) (string, string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	runGitCommand(t, "", "init", "--bare", "--quiet", bare)
	controller := filepath.Join(t.TempDir(), "controller")
	runGitCommand(t, "", "init", "--quiet", controller)
	runGitCommand(t, controller, "config", "user.email", "test@example.com")
	runGitCommand(t, controller, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(controller, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(controller, ".gitignore"), []byte(".awdev/issues/\n.awdev/worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, controller, "add", "README.md", ".gitignore")
	runGitCommand(t, controller, "commit", "--quiet", "-m", "initial")
	runGitCommand(t, controller, "branch", "-M", "main")
	remoteURL := "https://github.com/owner/repository.git"
	runGitCommand(t, controller, "remote", "add", "origin", remoteURL)
	runGitCommand(t, controller, "config", "url.file://"+filepath.ToSlash(bare)+"/.insteadOf", remoteURL)
	runGitCommand(t, controller, "push", "--quiet", "-u", "origin", "main")
	if err := os.MkdirAll(filepath.Join(controller, ".awdev", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return controller, gitOutput(t, controller, "rev-parse", "HEAD")
}

func runGitCommand(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

type scriptedGitRunner struct {
	requests []processrun.Request
	stdout   []string
}

func (runner *scriptedGitRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	runner.requests = append(runner.requests, request)
	index := len(runner.requests) - 1
	return processrun.Result{Stdout: []byte(runner.stdout[index])}, nil
}

type recordingGitRunner struct {
	delegate processrun.Runner
	requests []processrun.Request
}

func (runner *recordingGitRunner) Run(ctx context.Context, request processrun.Request) (processrun.Result, error) {
	runner.requests = append(runner.requests, request)
	return runner.delegate.Run(ctx, request)
}

func assertSafeGitRequests(t *testing.T, requests []processrun.Request, hostileTitle string, worktree gitrepo.Worktree) {
	t.Helper()
	fetchIndex, addIndex := -1, -1
	for index, request := range requests {
		if len(request.Argv) == 0 || request.Argv[0] != "git" {
			t.Fatalf("unsafe executable argv: %#v", request.Argv)
		}
		if request.StdoutLimit <= 0 || request.StderrLimit <= 0 || request.Environment["GIT_TERMINAL_PROMPT"] != "0" {
			t.Fatalf("incomplete safe Git request: %#v", request)
		}
		for _, argument := range request.Argv {
			if argument == hostileTitle {
				t.Fatalf("raw hostile title reached Git argv: %#v", request.Argv)
			}
		}
		if containsSequence(request.Argv, "fetch", "--no-tags", "origin") {
			fetchIndex = index
		}
		if containsSequence(request.Argv, "worktree", "add", "-b") {
			addIndex = index
			want := []string{"git", "worktree", "add", "-b", worktree.Branch, worktree.AbsolutePath, worktree.BaseSHA}
			if !reflect.DeepEqual(request.Argv, want) {
				t.Fatalf("worktree add argv = %#v, want %#v", request.Argv, want)
			}
		}
	}
	if fetchIndex < 0 || addIndex <= fetchIndex {
		t.Fatalf("Git ordering did not fetch before creation: fetch=%d add=%d", fetchIndex, addIndex)
	}
}

func containsSequence(values []string, sequence ...string) bool {
	if len(sequence) > len(values) {
		return false
	}
	for start := 0; start <= len(values)-len(sequence); start++ {
		if reflect.DeepEqual(values[start:start+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
