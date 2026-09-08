package workflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/checks"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestResumeGitHubWithoutAnswerLeavesManifestBytesAndContinuationUntouched(t *testing.T) {
	root := t.TempDir()
	manifest := blockedResumeManifest(state.PhaseSpec)
	prepareResumeState(t, root, &manifest)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	manifestPath, _ := state.ManifestPath(root, manifest.WorkflowID)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	continuation := &fakeResumeContinuation{store: store}
	comments := &fakeCommentGateway{comments: []githubapi.IssueComment{blockerIssueComment(manifest)}}
	service := workflow.NewResumeService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}}, state.NewManifestReader(),
		state.NewTransitionService(store), comments, nil, continuation,
	)

	result, err := service.ResumeGitHub(context.Background(), root, 17)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != workflow.ResumeWaiting || result.Manifest.Status != state.StatusBlocked || continuation.called {
		t.Fatalf("result = %#v, continuation called = %t", result, continuation.called)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("waiting resume changed manifest bytes\nbefore: %s\nafter: %s", before, after)
	}
	if !reflect.DeepEqual(events, []string{"lock:gh-17", "unlock"}) {
		t.Fatalf("lock events = %v", events)
	}
}

func TestResumeGitHubAcceptsLaterHumanReplyFromWorkflowActor(t *testing.T) {
	root := t.TempDir()
	manifest := blockedResumeManifest(state.PhaseSpec)
	prepareResumeState(t, root, &manifest)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	blocker := blockerIssueComment(manifest)
	comments := &fakeCommentGateway{comments: []githubapi.IssueComment{
		blocker,
		{
			ID: "5570957701", URL: commentURL("5570957701"),
			Author: githubapi.CommentAuthor{Login: manifest.Actor, Type: githubapi.CommentAuthorTypeUser},
			Body:   "Orange", CreatedAt: blocker.CreatedAt.Add(82 * time.Second),
		},
	}}
	continuation := &fakeResumeContinuation{store: store}
	events := []string{}
	service := workflow.NewResumeService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}}, state.NewManifestReader(),
		state.NewTransitionService(store), comments, nil, continuation,
	)

	result, err := service.ResumeGitHub(context.Background(), root, 17)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.Outcome != workflow.ResumeContinued || result.Answer == nil || result.Answer.Body != "Orange" {
		t.Fatalf("same-login human reply was not accepted: %#v", result)
	}
}

func TestResumeGitHubSelectsEarliestEligibleHumanReplyAndContinuesRecordedPhase(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseSpec, state.PhaseImplementation, state.PhaseReview} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			manifest := blockedResumeManifest(phase)
			prepareResumeState(t, root, &manifest)
			store := state.NewStore()
			if err := store.Save(root, manifest); err != nil {
				t.Fatal(err)
			}
			blocker := blockerIssueComment(manifest)
			answerTime := blocker.CreatedAt.Add(time.Minute)
			malformedTime := blocker.CreatedAt.Add(10 * time.Second)
			comments := &fakeCommentGateway{comments: []githubapi.IssueComment{
				{ID: "999", URL: commentURL("999"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "too old", CreatedAt: blocker.CreatedAt.Add(-time.Second)},
				{ID: "99", URL: commentURL("99"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "same timestamp but before blocker", CreatedAt: blocker.CreatedAt},
				blocker,
				{ID: "not-a-number", URL: commentURL("not-a-number"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "malformed id", CreatedAt: malformedTime},
				{ID: "160", URL: "https://example.com/owner/repository/issues/17#issuecomment-160", Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "malformed URL", CreatedAt: malformedTime},
				{ID: "161", URL: commentURL("999"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "mismatched URL identity", CreatedAt: malformedTime},
				{ID: "162", URL: commentURL("162"), Author: githubapi.CommentAuthor{Type: "User"}, Body: "missing author", CreatedAt: malformedTime},
				{ID: "163", URL: commentURL("163"), Author: githubapi.CommentAuthor{Login: "human"}, Body: "missing author type", CreatedAt: malformedTime},
				{ID: "164", URL: commentURL("164"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "missing timestamp"},
				{ID: "150", URL: commentURL("150"), Author: githubapi.CommentAuthor{Login: "dependabot[bot]", Type: "Bot"}, Body: "bot answer", CreatedAt: answerTime.Add(-time.Second)},
				{ID: "201", URL: commentURL("201"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "", CreatedAt: answerTime.Add(-time.Second)},
				{ID: "203", URL: commentURL("203"), Author: githubapi.CommentAuthor{Login: "second", Type: "User"}, Body: "later stable id", CreatedAt: answerTime},
				{ID: "202", URL: commentURL("202"), Author: githubapi.CommentAuthor{Login: "first", Type: "User"}, Body: "Use option A", CreatedAt: answerTime},
				{ID: "204", URL: commentURL("204"), Author: githubapi.CommentAuthor{Login: "later", Type: "User"}, Body: "later timestamp", CreatedAt: answerTime.Add(time.Minute)},
			}}
			continuation := &fakeResumeContinuation{store: store}
			events := []string{}
			service := workflow.NewResumeService(
				fakeLocker{events: &events, lock: &fakeLock{events: &events}}, state.NewManifestReader(),
				state.NewTransitionService(store), comments, nil, continuation,
			)

			result, err := service.ResumeGitHub(context.Background(), root, 17)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if result.Outcome != workflow.ResumeContinued || !continuation.called || continuation.phase != phase {
				t.Fatalf("result = %#v, continuation = %#v", result, continuation)
			}
			if continuation.answer == nil || continuation.answer.ID != "202" || continuation.answer.Body != "Use option A" || continuation.answer.Author != "first" {
				t.Fatalf("selected answer = %#v", continuation.answer)
			}
		})
	}
}

func TestResumeContinuationDispatchesTheRecordedPhaseAndNormalDownstreamGates(t *testing.T) {
	for _, test := range []struct {
		phase state.Phase
		want  []string
	}{
		{phase: state.PhaseSpec, want: []string{"spec:resume", "implementation:implement", "review:review"}},
		{phase: state.PhaseImplementation, want: []string{"implementation:resume", "review:review"}},
		{phase: state.PhaseReview, want: []string{"review:resume"}},
	} {
		t.Run(string(test.phase), func(t *testing.T) {
			events := []string{}
			specification := &fakeSpecificationContinuation{events: &events}
			implementation := &fakeImplementationContinuation{events: &events}
			reviewer := &fakeReviewContinuation{events: &events}
			service := workflow.NewResumeContinuationService(specification, implementation, reviewer)
			result, err := service.ContinueResume(context.Background(), "/repo", fixedWorkflowID, test.phase)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, test.want) || result.Manifest.Phase != state.PhaseReview {
				t.Fatalf("events=%v result=%#v", events, result)
			}
		})
	}
}

func TestResumeContinuationTechnicalErrorBecomesFailed(t *testing.T) {
	root := t.TempDir()
	manifest := blockedResumeManifest(state.PhaseImplementation)
	prepareResumeState(t, root, &manifest)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	blocker := blockerIssueComment(manifest)
	comments := &fakeCommentGateway{comments: []githubapi.IssueComment{
		blocker,
		{ID: "101", URL: commentURL("101"), Author: githubapi.CommentAuthor{Login: "human", Type: "User"}, Body: "Use option A", CreatedAt: blocker.CreatedAt.Add(time.Minute)},
	}}
	events := []string{}
	service := workflow.NewResumeService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}}, state.NewManifestReader(), state.NewTransitionService(store), comments, nil,
		&fakeResumeContinuation{store: store, err: errors.New("agent crashed")},
	)

	result, err := service.ResumeGitHub(context.Background(), root, 17)
	if err == nil || result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

type fakeResumeContinuation struct {
	store  *state.Store
	called bool
	phase  state.Phase
	answer *state.BlockerAnswer
	err    error
}

type fakeSpecificationContinuation struct{ events *[]string }

func (fake *fakeSpecificationContinuation) Resume(context.Context, string, string) (workflow.SpecificationResult, error) {
	*fake.events = append(*fake.events, "spec:resume")
	return workflow.SpecificationResult{Manifest: state.Manifest{Phase: state.PhaseSpec}}, nil
}

type fakeImplementationContinuation struct{ events *[]string }

func (fake *fakeImplementationContinuation) Implement(context.Context, string, string) (workflow.ImplementationResult, error) {
	*fake.events = append(*fake.events, "implementation:implement")
	return workflow.ImplementationResult{Manifest: state.Manifest{Phase: state.PhaseImplementation}, CheckResults: []checks.Result{{Name: "checks", ExitCode: 0}}, CheckedState: gitrepo.WorktreeBaseline{}}, nil
}

func (fake *fakeImplementationContinuation) Resume(context.Context, string, string) (workflow.ImplementationResult, error) {
	*fake.events = append(*fake.events, "implementation:resume")
	return workflow.ImplementationResult{Manifest: state.Manifest{Phase: state.PhaseImplementation}, CheckResults: []checks.Result{{Name: "checks", ExitCode: 0}}, CheckedState: gitrepo.WorktreeBaseline{}}, nil
}

type fakeReviewContinuation struct{ events *[]string }

func (fake *fakeReviewContinuation) Review(context.Context, string, string, []checks.Result, gitrepo.WorktreeBaseline) (workflow.ReviewResult, error) {
	*fake.events = append(*fake.events, "review:review")
	return workflow.ReviewResult{Manifest: state.Manifest{Phase: state.PhaseReview}}, nil
}

func (fake *fakeReviewContinuation) Resume(context.Context, string, string) (workflow.ReviewResult, error) {
	*fake.events = append(*fake.events, "review:resume")
	return workflow.ReviewResult{Manifest: state.Manifest{Phase: state.PhaseReview}}, nil
}

func (continuation *fakeResumeContinuation) ContinueResume(_ context.Context, root, workflowID string, phase state.Phase) (workflow.ResumeContinuation, error) {
	continuation.called = true
	continuation.phase = phase
	manifest, err := continuation.store.Read(root, workflowID)
	if err != nil {
		return workflow.ResumeContinuation{}, err
	}
	if manifest.Blocker != nil && manifest.Blocker.Answer != nil {
		answer := *manifest.Blocker.Answer
		continuation.answer = &answer
	}
	return workflow.ResumeContinuation{Manifest: manifest}, continuation.err
}

func blockedResumeManifest(phase state.Phase) state.Manifest {
	manifest := blockerManifest(phase, state.StatusBlocked)
	if phase == state.PhaseSpec {
		manifest.SpecificationPath = ""
	}
	manifest.BlockerSequence = 1
	manifest.Blocker = &state.Blocker{
		ID: "blocker-1", Phase: phase, Question: "Which behavior?", Actor: manifest.Actor,
		Marker: state.BlockerMarker(manifest.WorkflowID, "blocker-1"), CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Comment: &state.SourceReference{ID: "100", URL: commentURL("100")},
	}
	if phase == state.PhaseReview {
		manifest.Review = &state.ReviewCounters{Attempt: 3, MaxAttempts: 3}
	}
	return manifest
}

func prepareResumeState(t *testing.T, root string, manifest *state.Manifest) {
	t.Helper()
	worktree := filepath.Join(root, filepath.FromSlash(manifest.Worktree))
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest.SpecificationPath == "" {
		return
	}
	path := filepath.Join(worktree, filepath.FromSlash(manifest.SpecificationPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func blockerIssueComment(manifest state.Manifest) githubapi.IssueComment {
	return githubapi.IssueComment{
		ID: manifest.Blocker.Comment.ID, URL: manifest.Blocker.Comment.URL,
		Author: githubapi.CommentAuthor{Login: manifest.Actor, Type: "User"}, Body: manifest.Blocker.Question + "\n" + manifest.Blocker.Marker,
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 30, 0, time.UTC),
	}
}

func commentURL(id string) string {
	return "https://github.com/owner/repository/issues/17#issuecomment-" + id
}

func answeredBlocker(phase state.Phase) *state.Blocker {
	return &state.Blocker{
		ID: "blocker-1", Phase: phase, Question: "Which behavior?", Actor: "octocat",
		Marker: state.BlockerMarker(fixedWorkflowID, "blocker-1"), CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Comment: &state.SourceReference{ID: "100", URL: commentURL("100")},
		Answer:  &state.BlockerAnswer{ID: "101", URL: commentURL("101"), Body: "Use option A", Author: "human", CreatedAt: time.Date(2026, 9, 1, 12, 2, 0, 0, time.UTC)},
	}
}
