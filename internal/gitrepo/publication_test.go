package gitrepo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestPublicationManagerCommitsSpecificationAndImplementationThenPushesExactBranch(t *testing.T) {
	controller, baseSHA := newRemoteRepository(t)
	worktree, err := gitrepo.NewWorktreeManager("git", processrun.NewRunner()).Prepare(context.Background(), gitrepo.WorktreeRequest{
		ControllerRoot: controller, RepositoryIdentity: "owner/repository", DefaultBranch: "main", IssueNumber: 17, IssueTitle: "title",
	})
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}
	specification := filepath.Join(worktree.AbsolutePath, ".awdev", "specs", worktree.Branch+".md")
	if err := os.MkdirAll(filepath.Dir(specification), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specification, []byte("# Specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree.AbsolutePath, "implementation.go"), []byte("package implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager := gitrepo.NewPublicationManager("git", processrun.NewRunner())
	request := gitrepo.PublicationRequest{
		Worktree: worktree.AbsolutePath, Branch: worktree.Branch, BaseSHA: baseSHA,
		SpecificationPath: ".awdev/specs/" + worktree.Branch + ".md", CommitMessage: "awdev: implement issue #17",
	}
	treeSHA, err := manager.Stage(context.Background(), request)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	request.ExpectedTreeSHA = treeSHA
	commitSHA, err := manager.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	crashRecoveredSHA, err := manager.Commit(context.Background(), request)
	if err != nil {
		t.Fatalf("recover commit created before identity persistence: %v", err)
	}
	if crashRecoveredSHA != commitSHA {
		t.Fatalf("recovered commit = %s, want %s", crashRecoveredSHA, commitSHA)
	}
	unrecorded := request
	unrecorded.ExpectedTreeSHA = ""
	if _, err := manager.Commit(context.Background(), unrecorded); err == nil {
		t.Fatal("unrecorded preexisting commit was accepted as controller-owned")
	}
	request.ExpectedCommitSHA = commitSHA
	if err := manager.Push(context.Background(), worktree.AbsolutePath, worktree.Branch); err != nil {
		t.Fatalf("push: %v", err)
	}

	if got := gitOutput(t, worktree.AbsolutePath, "branch", "--show-current"); got != worktree.Branch {
		t.Fatalf("checked-out branch = %q, want %q", got, worktree.Branch)
	}
	if got := gitOutput(t, worktree.AbsolutePath, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"); got != ".awdev/specs/"+worktree.Branch+".md\nimplementation.go" {
		t.Fatalf("commit paths = %q", got)
	}
	if got := gitOutput(t, controller, "ls-remote", "--heads", "origin", worktree.Branch); got == "" {
		t.Fatal("recorded branch was not pushed")
	}
	firstHead := gitOutput(t, worktree.AbsolutePath, "rev-parse", "HEAD")
	if _, err := manager.Commit(context.Background(), request); err != nil {
		t.Fatalf("idempotent commit reconciliation: %v", err)
	}
	if got := gitOutput(t, worktree.AbsolutePath, "rev-parse", "HEAD"); got != firstHead {
		t.Fatalf("retry created a second commit: got %s, want %s", got, firstHead)
	}
}

func TestPublicationManagerRejectsNoChangesAndWrongBranch(t *testing.T) {
	controller, baseSHA := newRemoteRepository(t)
	manager := gitrepo.NewPublicationManager("git", processrun.NewRunner())

	t.Run("no changes", func(t *testing.T) {
		request := gitrepo.PublicationRequest{Worktree: controller, Branch: "main", BaseSHA: baseSHA, SpecificationPath: ".awdev/specs/missing.md", CommitMessage: "message"}
		if _, err := manager.Stage(context.Background(), request); err == nil {
			t.Fatal("empty diff was accepted")
		}
	})

	t.Run("wrong branch", func(t *testing.T) {
		runGitCommand(t, controller, "checkout", "-b", "other")
		if err := os.WriteFile(filepath.Join(controller, "change.txt"), []byte("change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		request := gitrepo.PublicationRequest{Worktree: controller, Branch: "main", BaseSHA: baseSHA, SpecificationPath: "change.txt", CommitMessage: "message"}
		if _, err := manager.Stage(context.Background(), request); err == nil {
			t.Fatal("wrong branch was accepted")
		}
	})
}

func TestPublicationManagerRejectsWorktreeChangedAfterTreeIntent(t *testing.T) {
	controller, baseSHA := newRemoteRepository(t)
	specification := filepath.Join(controller, ".awdev", "specs", "main.md")
	if err := os.MkdirAll(filepath.Dir(specification), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specification, []byte("# approved specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	implementation := filepath.Join(controller, "implementation.go")
	if err := os.WriteFile(implementation, []byte("package implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := gitrepo.NewPublicationManager("git", processrun.NewRunner())
	request := gitrepo.PublicationRequest{
		Worktree: controller, Branch: "main", BaseSHA: baseSHA,
		SpecificationPath: ".awdev/specs/main.md", CommitMessage: "awdev: implement issue #17",
	}
	treeSHA, err := manager.Stage(context.Background(), request)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	request.ExpectedTreeSHA = treeSHA
	if err := os.WriteFile(implementation, []byte("package implementation\n\n// changed after intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Commit(context.Background(), request); err == nil || !strings.Contains(err.Error(), "durable commit intent") {
		t.Fatalf("changed checked content was accepted: %v", err)
	}
	if got := gitOutput(t, controller, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("branch advanced after intent mismatch: got %s, want %s", got, baseSHA)
	}
}

func TestPublicationManagerReportsStagingAndCommitFailures(t *testing.T) {
	baseSHA := strings.Repeat("a", 40)
	request := gitrepo.PublicationRequest{Worktree: "/repo", Branch: "gh-17-title", BaseSHA: baseSHA, SpecificationPath: ".awdev/specs/gh-17-title.md", CommitMessage: "message"}
	exit := func() error {
		return &processrun.ExitError{Argv: []string{"git"}, Result: processrun.Result{ExitCode: 1}}
	}

	t.Run("staging", func(t *testing.T) {
		runner := &publicationScriptRunner{responses: []publicationScriptResponse{{stdout: "gh-17-title"}, {stdout: baseSHA}, {err: exit()}}}
		_, err := gitrepo.NewPublicationManager("git", runner).Stage(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "stage publication changes") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		request.ExpectedTreeSHA = strings.Repeat("c", 40)
		runner := &publicationScriptRunner{responses: []publicationScriptResponse{{stdout: "gh-17-title"}, {stdout: baseSHA}, {}, {stdout: request.ExpectedTreeSHA}, {err: exit()}, {err: exit()}, {err: exit()}}}
		_, err := gitrepo.NewPublicationManager("git", runner).Commit(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "create controller-owned commit") {
			t.Fatalf("error = %v", err)
		}
	})
}

type publicationScriptResponse struct {
	stdout string
	err    error
}

type publicationScriptRunner struct {
	responses []publicationScriptResponse
	index     int
}

func (runner *publicationScriptRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	if runner.index >= len(runner.responses) {
		return processrun.Result{}, errors.New("unexpected Git request")
	}
	response := runner.responses[runner.index]
	runner.index++
	result := processrun.Result{Stdout: []byte(response.stdout)}
	if exitError, ok := response.err.(*processrun.ExitError); ok {
		exitError.Argv = request.Argv
		result = exitError.Result
	}
	return result, response.err
}
