package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestRunGitHubOrdersLockStateFetchValidationAndContinuation(t *testing.T) {
	events := []string{}
	lock := &fakeLock{events: &events}
	locker := fakeLocker{events: &events, lock: lock}
	existing := fakeExisting{events: &events}
	fetcher := fakeFetcher{events: &events, snapshot: validSnapshot()}
	next := &fakeBootstrapper{events: &events, result: gitrepo.Worktree{
		Branch:            "gh-17-a-title",
		BaseSHA:           strings.Repeat("a", 40),
		AbsolutePath:      "/repo/.awdev/worktrees/gh-17-a-title",
		SpecificationPath: ".awdev/specs/gh-17.md",
	}}
	service := workflow.NewRunService(locker, existing, fetcher, next)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Outcome != workflow.RunReady || result.WorkflowID != "gh-17" || result.Snapshot.Issue.Number != 17 {
		t.Fatalf("result = %#v", result)
	}
	if result.Worktree != next.result {
		t.Fatalf("result worktree = %#v, want %#v", result.Worktree, next.result)
	}
	want := []string{"state:gh-17", "lock:gh-17", "state:gh-17", "github:17", "continue:gh-17", "unlock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if next.bootstrap.Snapshot.Issue.Body != validSnapshot().Issue.Body {
		t.Fatal("typed issue snapshot was not carried to the next bootstrap slice")
	}
}

func TestRunGitHubRejectsInvalidSnapshotBeforeContinuation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*githubapi.Snapshot)
		wantText string
	}{
		{name: "wrong number", mutate: func(value *githubapi.Snapshot) { value.Issue.Number = 18 }, wantText: "expected #17"},
		{name: "closed", mutate: func(value *githubapi.Snapshot) { value.Issue.State = "CLOSED" }, wantText: "is closed"},
		{name: "missing repository", mutate: func(value *githubapi.Snapshot) { value.Repository.NameWithOwner = "" }, wantText: "repository"},
		{name: "missing actor", mutate: func(value *githubapi.Snapshot) { value.Actor.Login = "" }, wantText: "actor"},
		{name: "missing update timestamp", mutate: func(value *githubapi.Snapshot) { value.Issue.UpdatedAt = time.Time{} }, wantText: "updatedAt"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			snapshot := validSnapshot()
			test.mutate(&snapshot)
			next := &fakeBootstrapper{events: &events}
			service := workflow.NewRunService(
				fakeLocker{events: &events, lock: &fakeLock{events: &events}},
				fakeExisting{events: &events},
				fakeFetcher{events: &events, snapshot: snapshot},
				next,
			)
			_, err := service.RunGitHub(context.Background(), "/repo", 17)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %v, want %q", err, test.wantText)
			}
			if next.called {
				t.Fatal("next bootstrap slice ran with invalid GitHub data")
			}
			if events[len(events)-1] != "unlock" {
				t.Fatalf("lock was not released: %v", events)
			}
		})
	}
}

func TestRunGitHubExistingManifestShortCircuitsFetchAndContinuation(t *testing.T) {
	for _, status := range []string{"running", "blocked", "failed", "done"} {
		t.Run(status, func(t *testing.T) {
			events := []string{}
			fetcher := fakeFetcher{events: &events, err: errors.New("must not fetch")}
			next := &fakeBootstrapper{events: &events}
			service := workflow.NewRunService(
				fakeLocker{events: &events, lock: &fakeLock{events: &events}},
				fakeExisting{events: &events, workflow: state.ExistingWorkflow{Exists: true, Phase: "spec", Status: state.Status(status)}},
				fetcher,
				next,
			)
			result, err := service.RunGitHub(context.Background(), "/repo", 17)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Outcome != workflow.RunExisting || string(result.Existing.Status) != status {
				t.Fatalf("result = %#v", result)
			}
			want := []string{"state:gh-17"}
			if !reflect.DeepEqual(events, want) || next.called {
				t.Fatalf("duplicate run side effects: events=%v next=%v", events, next.called)
			}
		})
	}
}

func TestRunGitHubRechecksManifestAfterAcquiringLock(t *testing.T) {
	events := []string{}
	existing := &sequenceExisting{
		events: &events,
		workflows: []state.ExistingWorkflow{
			{},
			{Exists: true, Phase: "spec", Status: "running"},
		},
	}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		existing,
		fakeFetcher{events: &events, err: errors.New("must not fetch")},
		&fakeBootstrapper{events: &events},
	)
	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Outcome != workflow.RunExisting || result.Existing.Status != "running" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"state:gh-17", "lock:gh-17", "state:gh-17", "unlock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunGitHubRequiresWorktreeBootstrapperForNewWorkflow(t *testing.T) {
	events := []string{}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		fakeExisting{events: &events},
		fakeFetcher{events: &events, snapshot: validSnapshot()},
		nil,
	)
	_, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err == nil || !strings.Contains(err.Error(), "not fully configured") {
		t.Fatalf("error = %v, want configuration failure", err)
	}
	if len(events) != 0 {
		t.Fatalf("incomplete service performed side effects: %v", events)
	}
}

func validSnapshot() githubapi.Snapshot {
	return githubapi.Snapshot{
		Repository: githubapi.Repository{NameWithOwner: "owner/repository", DefaultBranch: "main"},
		Actor:      githubapi.Actor{Login: "octocat"},
		Issue: githubapi.Issue{
			Number:    17,
			Title:     "A title",
			Body:      "line one\n<!-- data -->\nUnicode — $() ;",
			URL:       "https://github.com/owner/repository/issues/17",
			State:     "OPEN",
			UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		},
	}
}

type fakeLocker struct {
	events *[]string
	lock   state.Lock
}

func (locker fakeLocker) Acquire(_ context.Context, _ string, workflowID string) (state.Lock, error) {
	*locker.events = append(*locker.events, "lock:"+workflowID)
	return locker.lock, nil
}

type fakeLock struct{ events *[]string }

func (lock *fakeLock) Release() error {
	*lock.events = append(*lock.events, "unlock")
	return nil
}

type fakeExisting struct {
	events   *[]string
	workflow state.ExistingWorkflow
}

type sequenceExisting struct {
	events    *[]string
	workflows []state.ExistingWorkflow
}

func (existing *sequenceExisting) ReadExisting(_ string, workflowID string) (state.ExistingWorkflow, error) {
	*existing.events = append(*existing.events, "state:"+workflowID)
	workflow := existing.workflows[0]
	existing.workflows = existing.workflows[1:]
	return workflow, nil
}

func (existing fakeExisting) ReadExisting(_ string, workflowID string) (state.ExistingWorkflow, error) {
	*existing.events = append(*existing.events, "state:"+workflowID)
	return existing.workflow, nil
}

type fakeFetcher struct {
	events   *[]string
	snapshot githubapi.Snapshot
	err      error
}

func (fetcher fakeFetcher) Fetch(_ context.Context, _ string, issueNumber int) (githubapi.Snapshot, error) {
	*fetcher.events = append(*fetcher.events, "github:"+fmtInt(issueNumber))
	return fetcher.snapshot, fetcher.err
}

type fakeBootstrapper struct {
	events    *[]string
	called    bool
	bootstrap workflow.Bootstrap
	result    gitrepo.Worktree
	err       error
}

func (bootstrapper *fakeBootstrapper) Continue(_ context.Context, bootstrap workflow.Bootstrap) (gitrepo.Worktree, error) {
	bootstrapper.called = true
	bootstrapper.bootstrap = bootstrap
	*bootstrapper.events = append(*bootstrapper.events, "continue:"+bootstrap.WorkflowID)
	return bootstrapper.result, bootstrapper.err
}

func fmtInt(value int) string {
	if value == 17 {
		return "17"
	}
	return "unexpected"
}
