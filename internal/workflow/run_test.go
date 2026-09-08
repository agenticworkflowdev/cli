package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/checks"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

const fixedWorkflowID = "wf_0123456789abcdef0123456789abcdef"

func TestRunGitHubOrdersLockStateFetchValidationAndContinuation(t *testing.T) {
	events := []string{}
	lock := &fakeLock{events: &events}
	locker := fakeLocker{events: &events, lock: lock}
	existing := fakeExisting{events: &events}
	fetcher := fakeFetcher{events: &events, snapshot: validSnapshot()}
	next := &fakeBootstrapper{events: &events, result: gitrepo.Worktree{
		Branch:       "gh-17-a-title",
		BaseSHA:      strings.Repeat("a", 40),
		AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title",
	}}
	writer := &fakeManifestWriter{events: &events}
	specification := &fakeSpecificationCreator{events: &events, writer: writer}
	implementation := &fakeImplementationRunner{events: &events, writer: writer}
	reviewer := &fakeReviewer{events: &events, writer: writer}
	service := workflow.NewRunService(locker, existing, fetcher, next, writer, generateFixedWorkflowID, specification, implementation, reviewer)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Outcome != workflow.RunReady || result.WorkflowID != fixedWorkflowID || result.Snapshot.Issue.Number != 17 {
		t.Fatalf("result = %#v", result)
	}
	if result.Worktree != next.result {
		t.Fatalf("result worktree = %#v, want %#v", result.Worktree, next.result)
	}
	if result.Manifest == nil || result.Manifest.Issue.Body != validSnapshot().Issue.Body || result.Manifest.Phase != state.PhaseReview || result.Manifest.Status != state.StatusRunning || result.Manifest.SpecificationPath != ".awdev/specs/gh-17-a-title.md" {
		t.Fatalf("completed review manifest = %#v", result.Manifest)
	}
	want := []string{"state:17", "lock:gh-17", "state:17", "github:17", "continue", "manifest:" + fixedWorkflowID, "specification", "implementation", "review", "unlock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if next.bootstrap.Snapshot.Issue.Body != validSnapshot().Issue.Body {
		t.Fatal("typed issue snapshot was not carried to the next bootstrap slice")
	}
	if result.Implementation == nil || result.Implementation.Manifest.Phase != state.PhaseImplementation {
		t.Fatalf("implementation result = %#v", result.Implementation)
	}
	if result.Review == nil || result.Review.Manifest.Phase != state.PhaseReview {
		t.Fatalf("review result = %#v", result.Review)
	}
}

func TestRunGitHubDoesNotImplementABlockedSpecification(t *testing.T) {
	events := []string{}
	writer := &fakeManifestWriter{events: &events}
	implementation := &fakeImplementationRunner{events: &events, writer: writer}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		fakeExisting{events: &events},
		fakeFetcher{events: &events, snapshot: validSnapshot()},
		&fakeBootstrapper{events: &events, result: gitrepo.Worktree{Branch: "gh-17-a-title", BaseSHA: strings.Repeat("a", 40), AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title"}},
		writer,
		generateFixedWorkflowID,
		&fakeSpecificationCreator{events: &events, writer: writer, blocker: &workflow.BlockerRequest{Phase: state.PhaseSpec, Question: "Which API?"}},
		implementation,
		&fakeReviewer{events: &events, writer: writer},
		&fakeBlockerPublisher{events: &events, writer: writer},
	)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatal(err)
	}
	if result.Specification == nil || result.Specification.Blocker == nil || implementation.called {
		t.Fatalf("result = %#v, implementation called = %v", result, implementation.called)
	}
	if result.Manifest == nil || result.Manifest.Status != state.StatusBlocked || result.Manifest.Blocker == nil || result.Manifest.Blocker.Comment == nil {
		t.Fatalf("blocker was not published: %#v", result.Manifest)
	}
}

func TestRunGitHubDoesNotReviewABlockedImplementation(t *testing.T) {
	events := []string{}
	writer := &fakeManifestWriter{events: &events}
	implementation := &fakeImplementationRunner{
		events: &events, writer: writer,
		blocker: &workflow.BlockerRequest{Phase: state.PhaseImplementation, Question: "Which API?"},
	}
	reviewer := &fakeReviewer{events: &events, writer: writer}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}}, fakeExisting{events: &events},
		fakeFetcher{events: &events, snapshot: validSnapshot()},
		&fakeBootstrapper{events: &events, result: gitrepo.Worktree{Branch: "gh-17-a-title", BaseSHA: strings.Repeat("a", 40), AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title"}},
		writer, generateFixedWorkflowID, &fakeSpecificationCreator{events: &events, writer: writer}, implementation, reviewer,
		&fakeBlockerPublisher{events: &events, writer: writer},
	)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatal(err)
	}
	if result.Implementation == nil || result.Implementation.Blocker == nil || reviewer.called || result.Review != nil {
		t.Fatalf("result = %#v, reviewer called = %t", result, reviewer.called)
	}
	if result.Manifest == nil || result.Manifest.Status != state.StatusBlocked {
		t.Fatalf("implementation blocker was not published: %#v", result.Manifest)
	}
}

func TestRunGitHubReconcilesPendingBlockerIntentAfterReentry(t *testing.T) {
	events := []string{}
	pending := state.Manifest{
		WorkflowID: fixedWorkflowID, Phase: state.PhaseSpec, Status: state.StatusRunning,
		Blocker: &state.Blocker{Phase: state.PhaseSpec, Question: "Which API?"},
	}
	blocked := pending
	blocked.Status = state.StatusBlocked
	publisher := &fakeBlockerPublisher{events: &events, result: &blocked}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		&sequenceExisting{events: &events, workflows: []state.ExistingWorkflow{{Exists: true, Manifest: &pending}, {Exists: true, Manifest: &pending}}},
		fakeFetcher{events: &events, err: errors.New("must not fetch")}, &fakeBootstrapper{events: &events},
		&fakeManifestWriter{events: &events}, generateFixedWorkflowID,
		&fakeSpecificationCreator{events: &events}, &fakeImplementationRunner{events: &events}, &fakeReviewer{events: &events}, publisher,
	)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != workflow.RunExisting || result.Existing.Manifest == nil || result.Existing.Manifest.Status != state.StatusBlocked {
		t.Fatalf("result = %#v", result)
	}
	if want := []string{"state:17", "lock:gh-17", "state:17", "blocker", "unlock"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunGitHubReturnsFailedImplementationEvidenceForDiagnostics(t *testing.T) {
	events := []string{}
	writer := &fakeManifestWriter{events: &events}
	failed := state.Manifest{WorkflowID: fixedWorkflowID, Phase: state.PhaseImplementation, Status: state.StatusFailed, LastError: &state.WorkflowError{Code: "technical_failure", Message: "checks failed"}}
	implementation := &fakeImplementationRunner{
		events: &events, writer: writer, result: workflow.ImplementationResult{Manifest: failed}, err: errors.New("checks failed"),
	}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}}, fakeExisting{events: &events},
		fakeFetcher{events: &events, snapshot: validSnapshot()},
		&fakeBootstrapper{events: &events, result: gitrepo.Worktree{Branch: "gh-17-a-title", BaseSHA: strings.Repeat("a", 40), AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title"}},
		writer, generateFixedWorkflowID, &fakeSpecificationCreator{events: &events, writer: writer}, implementation,
		&fakeReviewer{events: &events, writer: writer},
	)

	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err == nil || result.Implementation == nil || result.Manifest == nil || result.Manifest.Status != state.StatusFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
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
				&fakeManifestWriter{events: &events},
				generateFixedWorkflowID,
				&fakeSpecificationCreator{events: &events},
				&fakeImplementationRunner{events: &events},
				&fakeReviewer{events: &events},
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
				fakeExisting{events: &events, workflow: state.ExistingWorkflow{Exists: true, Manifest: &state.Manifest{WorkflowID: fixedWorkflowID, Phase: "spec", Status: state.Status(status)}}},
				fetcher,
				next,
				&fakeManifestWriter{events: &events},
				generateFixedWorkflowID,
				&fakeSpecificationCreator{events: &events},
				&fakeImplementationRunner{events: &events},
				&fakeReviewer{events: &events},
			)
			result, err := service.RunGitHub(context.Background(), "/repo", 17)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.Outcome != workflow.RunExisting || result.Existing.Manifest == nil || string(result.Existing.Manifest.Status) != status {
				t.Fatalf("result = %#v", result)
			}
			want := []string{"state:17"}
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
			{Exists: true, Manifest: &state.Manifest{WorkflowID: fixedWorkflowID, Phase: "spec", Status: "running"}},
		},
	}
	service := workflow.NewRunService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		existing,
		fakeFetcher{events: &events, err: errors.New("must not fetch")},
		&fakeBootstrapper{events: &events},
		&fakeManifestWriter{events: &events},
		generateFixedWorkflowID,
		&fakeSpecificationCreator{events: &events},
		&fakeImplementationRunner{events: &events},
		&fakeReviewer{events: &events},
	)
	result, err := service.RunGitHub(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Outcome != workflow.RunExisting || result.Existing.Manifest == nil || result.Existing.Manifest.Status != "running" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"state:17", "lock:gh-17", "state:17", "unlock"}
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
		nil,
		generateFixedWorkflowID,
		nil,
		nil,
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

func TestRunGitHubPersistsOnlyAfterValidatedWorktreeAndReportsPersistenceFailure(t *testing.T) {
	t.Run("worktree failure", func(t *testing.T) {
		events := []string{}
		writer := &fakeManifestWriter{events: &events}
		service := workflow.NewRunService(
			fakeLocker{events: &events, lock: &fakeLock{events: &events}},
			fakeExisting{events: &events},
			fakeFetcher{events: &events, snapshot: validSnapshot()},
			&fakeBootstrapper{events: &events, err: errors.New("worktree invalid")},
			writer,
			generateFixedWorkflowID,
			&fakeSpecificationCreator{events: &events},
			&fakeImplementationRunner{events: &events},
			&fakeReviewer{events: &events},
		)
		if _, err := service.RunGitHub(context.Background(), "/repo", 17); err == nil {
			t.Fatal("worktree failure was ignored")
		}
		if writer.manifest.WorkflowID != "" {
			t.Fatal("manifest was written before worktree validation")
		}
	})

	t.Run("manifest failure", func(t *testing.T) {
		events := []string{}
		writer := &fakeManifestWriter{events: &events, err: errors.New("disk full")}
		service := workflow.NewRunService(
			fakeLocker{events: &events, lock: &fakeLock{events: &events}},
			fakeExisting{events: &events},
			fakeFetcher{events: &events, snapshot: validSnapshot()},
			&fakeBootstrapper{events: &events, result: gitrepo.Worktree{
				Branch: "gh-17-a-title", BaseSHA: strings.Repeat("a", 40), AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title",
			}},
			writer,
			generateFixedWorkflowID,
			&fakeSpecificationCreator{events: &events},
			&fakeImplementationRunner{events: &events},
			&fakeReviewer{events: &events},
		)
		if _, err := service.RunGitHub(context.Background(), "/repo", 17); err == nil || !strings.Contains(err.Error(), "persist initial workflow manifest") {
			t.Fatalf("error = %v, want persistence failure", err)
		}
		want := []string{"state:17", "lock:gh-17", "state:17", "github:17", "continue", "manifest:" + fixedWorkflowID, "unlock"}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("events = %v, want %v", events, want)
		}
	})
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

func (existing *sequenceExisting) ReadExisting(_ string, issueNumber int) (state.ExistingWorkflow, error) {
	*existing.events = append(*existing.events, "state:"+fmtInt(issueNumber))
	workflow := existing.workflows[0]
	existing.workflows = existing.workflows[1:]
	return workflow, nil
}

func (existing fakeExisting) ReadExisting(_ string, issueNumber int) (state.ExistingWorkflow, error) {
	*existing.events = append(*existing.events, "state:"+fmtInt(issueNumber))
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

type fakeManifestWriter struct {
	events   *[]string
	manifest state.Manifest
	err      error
}

func (writer *fakeManifestWriter) Save(_ string, manifest state.Manifest) error {
	writer.manifest = manifest
	*writer.events = append(*writer.events, "manifest:"+manifest.WorkflowID)
	return writer.err
}

type fakeSpecificationCreator struct {
	events  *[]string
	writer  *fakeManifestWriter
	called  bool
	err     error
	blocker *workflow.BlockerRequest
}

type fakeImplementationRunner struct {
	events  *[]string
	writer  *fakeManifestWriter
	called  bool
	result  workflow.ImplementationResult
	err     error
	blocker *workflow.BlockerRequest
	checks  []checks.Result
}

type fakeReviewer struct {
	events *[]string
	writer *fakeManifestWriter
	called bool
	result workflow.ReviewResult
	err    error
}

type fakeBlockerPublisher struct {
	events *[]string
	writer *fakeManifestWriter
	result *state.Manifest
}

func (publisher *fakeBlockerPublisher) Publish(_ context.Context, _ string, _ string, blocker workflow.BlockerRequest) (state.Manifest, error) {
	*publisher.events = append(*publisher.events, "blocker")
	manifest := state.Manifest{}
	if publisher.result != nil {
		manifest = *publisher.result
	} else {
		manifest = publisher.writer.manifest
	}
	manifest.Phase = blocker.Phase
	manifest.Status = state.StatusBlocked
	manifest.BlockerSequence++
	id := state.BlockerID(manifest.BlockerSequence)
	manifest.Blocker = &state.Blocker{
		ID: id, Phase: blocker.Phase, Question: blocker.Question, Actor: manifest.Actor,
		Marker: state.BlockerMarker(manifest.WorkflowID, id), CreatedAt: time.Now(),
		Comment: &state.SourceReference{ID: "101", URL: "https://github.com/owner/repository/issues/17#issuecomment-101"},
	}
	return manifest, nil
}

func (runner *fakeImplementationRunner) Implement(_ context.Context, _ string, workflowID string) (workflow.ImplementationResult, error) {
	runner.called = true
	*runner.events = append(*runner.events, "implementation")
	if runner.err != nil {
		return runner.result, runner.err
	}
	manifest := state.Manifest{WorkflowID: workflowID}
	if runner.writer != nil {
		manifest = runner.writer.manifest
	}
	manifest.Phase = state.PhaseImplementation
	manifest.Status = state.StatusRunning
	manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
	return workflow.ImplementationResult{Manifest: manifest, CheckResults: cloneResultsForReviewTest(runner.checks), Blocker: runner.blocker}, nil
}

func (reviewer *fakeReviewer) Review(_ context.Context, _ string, workflowID string, _ []checks.Result, _ gitrepo.WorktreeBaseline) (workflow.ReviewResult, error) {
	reviewer.called = true
	*reviewer.events = append(*reviewer.events, "review")
	if reviewer.err != nil || reviewer.result.Manifest.WorkflowID != "" {
		return reviewer.result, reviewer.err
	}
	manifest := state.Manifest{WorkflowID: workflowID}
	if reviewer.writer != nil {
		manifest = reviewer.writer.manifest
	}
	manifest.Phase = state.PhaseReview
	manifest.Status = state.StatusRunning
	manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
	manifest.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
	return workflow.ReviewResult{Manifest: manifest}, nil
}

func (creator *fakeSpecificationCreator) Create(_ context.Context, _ string, workflowID string) (workflow.SpecificationResult, error) {
	creator.called = true
	*creator.events = append(*creator.events, "specification")
	if creator.err != nil {
		return workflow.SpecificationResult{}, creator.err
	}
	manifest := state.Manifest{WorkflowID: workflowID}
	if creator.writer != nil {
		manifest = creator.writer.manifest
	}
	manifest.Phase = state.PhaseSpec
	manifest.Status = state.StatusRunning
	manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
	return workflow.SpecificationResult{Manifest: manifest, Blocker: creator.blocker}, nil
}

func (bootstrapper *fakeBootstrapper) Continue(_ context.Context, bootstrap workflow.Bootstrap) (gitrepo.Worktree, error) {
	bootstrapper.called = true
	bootstrapper.bootstrap = bootstrap
	*bootstrapper.events = append(*bootstrapper.events, "continue")
	return bootstrapper.result, bootstrapper.err
}

func fmtInt(value int) string {
	if value == 17 {
		return "17"
	}
	return "unexpected"
}

func generateFixedWorkflowID() (string, error) { return fixedWorkflowID, nil }
