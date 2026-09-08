package workflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/checks"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestPublicationFinalizesApprovedReviewInDurableOrder(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.github.created = githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}

	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if result.Manifest.Phase != state.PhaseDone || result.Manifest.Status != state.StatusDone || result.Manifest.PullRequest == nil || result.Manifest.PullRequest.Number != 23 {
		t.Fatalf("result manifest = %#v", result.Manifest)
	}
	want := []string{"read", "review", "inspect", "transition:pull_request/running", "capture", "checks", "inspect", "stage", "transition:pull_request/running", "commit", "transition:pull_request/running", "push", "list", "create", "transition:pull_request/running", "transition:done/done"}
	if !reflect.DeepEqual(fixture.events, want) {
		t.Fatalf("events = %v, want %v", fixture.events, want)
	}
	if fixture.github.request.Body != "Closes #17\n\nThis pull request was created by awdev for GitHub issue #17." {
		t.Fatalf("body = %q", fixture.github.request.Body)
	}
}

func TestPublicationReusesOneClosedPullRequestAndRejectsAmbiguity(t *testing.T) {
	t.Run("reuse", func(t *testing.T) {
		fixture := newPublicationFixture(t)
		fixture.github.listed = []githubapi.PullRequest{{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "CLOSED"}}
		result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
		if err != nil {
			t.Fatal(err)
		}
		if fixture.github.createCalled || result.Manifest.PullRequest == nil || result.Manifest.PullRequest.Number != 23 {
			t.Fatalf("result = %#v, create called = %t", result, fixture.github.createCalled)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		fixture := newPublicationFixture(t)
		match := githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}
		fixture.github.listed = []githubapi.PullRequest{match, {Number: 24, URL: "https://github.com/owner/repository/pull/24", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "CLOSED"}}
		result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
		if err == nil || !strings.Contains(err.Error(), "ambiguous") || result.Manifest.Phase != state.PhasePullRequest || result.Manifest.Status != state.StatusFailed {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		if fixture.github.createCalled {
			t.Fatal("created a pull request after ambiguous lookup")
		}
	})
}

func TestPublicationRedactsCredentialPatternsFromGeneratedText(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.manifest.Issue.Title = "Ship ghp_abcdefghijklmnopqrstuvwxyz0123456789 token=visible-secret"
	fixture.github.created = githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}
	if _, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fixture.github.request.Title, "ghp_") || strings.Contains(fixture.github.request.Title, "visible-secret") || !strings.Contains(fixture.github.request.Title, "[REDACTED]") {
		t.Fatalf("title was not redacted: %q", fixture.github.request.Title)
	}
}

func TestPublicationFailureIsDurableAndRetryRerunsChecksAndPush(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.git.pushErr = errors.New("network unavailable")
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || result.Manifest.Status != state.StatusFailed || result.Manifest.Phase != state.PhasePullRequest {
		t.Fatalf("result = %#v, error = %v", result, err)
	}

	fixture.git.pushErr = nil
	fixture.github.created = githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}
	fixture.events = nil
	result, err = fixture.service().Retry(context.Background(), fixture.root, fixture.manifest.WorkflowID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Manifest.Status != state.StatusDone {
		t.Fatalf("retry result = %#v", result)
	}
	wantPrefix := []string{"read", "transition:pull_request/running", "capture", "checks", "inspect", "commit", "push"}
	if len(fixture.events) < len(wantPrefix) || !reflect.DeepEqual(fixture.events[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("retry events = %v", fixture.events)
	}
}

func TestPublicationStopsBeforeGitWhenFinalChecksFail(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.checks.results = []checks.Result{{Name: "test", ExitCode: 1, Stderr: "failed"}}
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || result.Manifest.Phase != state.PhasePullRequest || result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	joined := strings.Join(fixture.events, ",")
	if strings.Contains(joined, "commit") || strings.Contains(joined, "push") {
		t.Fatalf("publication continued after failed final checks: %v", fixture.events)
	}
}

func TestPublicationRejectsWorktreeChangedAfterApprovalBeforePhaseTransition(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.worktree.changes = [][]string{{"edited-after-review.go"}}
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "changed after approval") || result.Manifest.Phase != state.PhaseReview || result.Manifest.Status != state.StatusFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if strings.Contains(strings.Join(fixture.events, ","), "pull_request/running") {
		t.Fatalf("entered publication with stale approval: %v", fixture.events)
	}
}

func TestPublicationRequiresGeneratedSpecificationAtFinalization(t *testing.T) {
	fixture := newPublicationFixture(t)
	if err := os.Remove(filepath.Join(fixture.root, filepath.FromSlash(fixture.manifest.Worktree), filepath.FromSlash(fixture.manifest.SpecificationPath))); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "specification") || result.Manifest.Status != state.StatusFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if strings.Contains(strings.Join(fixture.events, ","), "commit") {
		t.Fatalf("commit attempted without specification: %v", fixture.events)
	}
}

func TestPublicationRetryReusesPullRequestWhenLocalIdentityPersistenceFailed(t *testing.T) {
	fixture := newPublicationFixture(t)
	created := githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}
	fixture.github.created = created
	fixture.store.failPullRequestPersistence = true
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || result.Manifest.Status != state.StatusFailed || fixture.github.createCount != 1 {
		t.Fatalf("result = %#v, error = %v, creates = %d", result, err, fixture.github.createCount)
	}
	fixture.github.listed = []githubapi.PullRequest{created}
	result, err = fixture.service().Retry(context.Background(), fixture.root, fixture.manifest.WorkflowID)
	if err != nil || result.Manifest.Status != state.StatusDone || fixture.github.createCount != 1 {
		t.Fatalf("retry result = %#v, error = %v, creates = %d", result, err, fixture.github.createCount)
	}
}

func TestPublicationRetryRecoversCommitWhenIdentityPersistenceFailed(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.store.failCommitPersistence = true
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || result.Manifest.Status != state.StatusFailed || result.Manifest.CommitTreeSHA == "" || result.Manifest.CommitSHA != "" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if fixture.git.stageCount != 1 {
		t.Fatalf("stage count after failed persistence = %d, want 1", fixture.git.stageCount)
	}

	fixture.github.created = githubapi.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: fixture.manifest.Branch, HeadOwner: "owner", Base: "main", State: "OPEN"}
	result, err = fixture.service().Retry(context.Background(), fixture.root, fixture.manifest.WorkflowID)
	if err != nil || result.Manifest.Status != state.StatusDone || result.Manifest.CommitSHA == "" {
		t.Fatalf("retry result = %#v, error = %v", result, err)
	}
	if fixture.git.stageCount != 1 {
		t.Fatalf("retry restaged after durable tree intent: count = %d", fixture.git.stageCount)
	}
}

func TestPublicationRejectsIdentityMismatchedLookupWithoutCreating(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.github.listed = []githubapi.PullRequest{{
		Number: 23, URL: "https://github.com/owner/repository/pull/23", Head: "other-branch", HeadOwner: "owner", Base: "main", State: "OPEN",
	}}
	result, err := fixture.service().Finalize(context.Background(), fixture.root, fixture.manifest.WorkflowID, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "does not match") || result.Manifest.Status != state.StatusFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if fixture.github.createCalled {
		t.Fatal("created a pull request after identity mismatch")
	}
}

type publicationFixture struct {
	root     string
	manifest state.Manifest
	events   []string
	store    *fakePublicationStore
	checks   *fakePublicationChecks
	worktree *fakePublicationWorktree
	git      *fakePublicationGit
	github   *fakePublicationGitHub
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	root := t.TempDir()
	branch := "gh-17-title"
	worktree := filepath.Join(root, ".awdev", "worktrees", branch)
	specificationPath := filepath.Join(worktree, ".awdev", "specs", branch+".md")
	if err := os.MkdirAll(filepath.Dir(specificationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specificationPath, []byte("# spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion, WorkflowID: fixedWorkflowID, Source: state.SourceGitHub,
		Repository: "owner/repository", DefaultBranch: "main", Actor: "octocat",
		Issue: state.IssueSnapshot{Number: 17, Title: "A title", Body: "body", URL: "https://github.com/owner/repository/issues/17", State: "OPEN", UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)},
		Phase: state.PhaseReview, Status: state.StatusRunning, Branch: branch, BaseSHA: strings.Repeat("a", 40),
		Worktree: ".awdev/worktrees/" + branch, SpecificationPath: ".awdev/specs/" + branch + ".md", Review: &state.ReviewCounters{Attempt: 1, MaxAttempts: 3},
	}
	fixture := &publicationFixture{root: root, manifest: manifest}
	fixture.store = &fakePublicationStore{fixture: fixture, review: review.Result{Approved: true, Findings: []review.Finding{}}}
	fixture.checks = &fakePublicationChecks{fixture: fixture, results: []checks.Result{{Name: "test", ExitCode: 0}}}
	fixture.worktree = &fakePublicationWorktree{fixture: fixture}
	fixture.git = &fakePublicationGit{fixture: fixture}
	fixture.github = &fakePublicationGitHub{fixture: fixture}
	return fixture
}

func (fixture *publicationFixture) service() *workflow.PublicationService {
	return workflow.NewPublicationService(fixture.store, fixture.store, fixture.store, fixture.checks, fixture.worktree, fixture.git, fixture.github, []checks.Definition{{Name: "test", Command: []string{"go", "test", "./..."}, Timeout: time.Minute}})
}

type fakePublicationStore struct {
	fixture                    *publicationFixture
	review                     review.Result
	failPullRequestPersistence bool
	failCommitPersistence      bool
}

func (store *fakePublicationStore) Read(string, string) (state.Manifest, error) {
	store.fixture.events = append(store.fixture.events, "read")
	return store.fixture.manifest, nil
}

func (store *fakePublicationStore) ReadReview(string, string) (review.Result, error) {
	store.fixture.events = append(store.fixture.events, "review")
	return store.review, nil
}

func (store *fakePublicationStore) Transition(_ string, _ string, next state.Manifest) error {
	store.fixture.events = append(store.fixture.events, "transition:"+string(next.Phase)+"/"+string(next.Status))
	if store.failCommitPersistence && next.CommitSHA != "" && store.fixture.manifest.CommitSHA == "" {
		store.failCommitPersistence = false
		return errors.New("disk unavailable")
	}
	if store.failPullRequestPersistence && next.PullRequest != nil && store.fixture.manifest.PullRequest == nil {
		store.failPullRequestPersistence = false
		return errors.New("disk unavailable")
	}
	store.fixture.manifest = next
	return nil
}

type fakePublicationChecks struct {
	fixture *publicationFixture
	results []checks.Result
	err     error
}

func (runner *fakePublicationChecks) Run(context.Context, string, []checks.Definition) ([]checks.Result, error) {
	runner.fixture.events = append(runner.fixture.events, "checks")
	return runner.results, runner.err
}

type fakePublicationWorktree struct {
	fixture *publicationFixture
	changes [][]string
}

func (inspector *fakePublicationWorktree) Capture(context.Context, string) (gitrepo.WorktreeBaseline, error) {
	inspector.fixture.events = append(inspector.fixture.events, "capture")
	return gitrepo.WorktreeBaseline{}, nil
}

func (inspector *fakePublicationWorktree) Inspect(context.Context, string, gitrepo.WorktreeBaseline) ([]string, error) {
	inspector.fixture.events = append(inspector.fixture.events, "inspect")
	if len(inspector.changes) > 0 {
		result := inspector.changes[0]
		inspector.changes = inspector.changes[1:]
		return result, nil
	}
	return nil, nil
}

type fakePublicationGit struct {
	fixture    *publicationFixture
	pushErr    error
	stageCount int
}

func (publisher *fakePublicationGit) Stage(_ context.Context, request gitrepo.PublicationRequest) (string, error) {
	publisher.fixture.events = append(publisher.fixture.events, "stage")
	publisher.stageCount++
	if request.ExpectedTreeSHA != "" {
		return request.ExpectedTreeSHA, nil
	}
	return strings.Repeat("c", 40), nil
}

func (publisher *fakePublicationGit) Commit(_ context.Context, request gitrepo.PublicationRequest) (string, error) {
	publisher.fixture.events = append(publisher.fixture.events, "commit")
	if request.ExpectedCommitSHA != "" {
		return request.ExpectedCommitSHA, nil
	}
	return strings.Repeat("b", 40), nil
}

func (publisher *fakePublicationGit) Push(context.Context, string, string) error {
	publisher.fixture.events = append(publisher.fixture.events, "push")
	return publisher.pushErr
}

type fakePublicationGitHub struct {
	fixture      *publicationFixture
	listed       []githubapi.PullRequest
	created      githubapi.PullRequest
	request      githubapi.CreatePullRequestRequest
	createCalled bool
	createCount  int
}

func (client *fakePublicationGitHub) ListPullRequests(context.Context, string, string, string) ([]githubapi.PullRequest, error) {
	client.fixture.events = append(client.fixture.events, "list")
	return client.listed, nil
}

func (client *fakePublicationGitHub) CreatePullRequest(_ context.Context, _ string, request githubapi.CreatePullRequestRequest) (githubapi.PullRequest, error) {
	client.fixture.events = append(client.fixture.events, "create")
	client.createCalled = true
	client.createCount++
	client.request = request
	return client.created, nil
}
