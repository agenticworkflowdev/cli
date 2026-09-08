package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestBlockerPublicationPersistsRedactedIntentBeforePostingAndBlocksAfterReconciliation(t *testing.T) {
	manifest := blockerManifest(state.PhaseSpec, state.StatusRunning)
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{actor: manifest.Actor, createdAt: time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)}
	service := workflow.NewBlockerService(states, states, comments, func() time.Time {
		return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	})

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase:    state.PhaseSpec,
		Question: "Use token=super-secret-value, AWS_SECRET_ACCESS_KEY='cloud-secret', ghp_abcdefghijklmnopqrstuvwxyz123456, or sk-abcdefghijklmnopqrstuvwxyz?",
	})
	if err != nil {
		t.Fatalf("publish blocker: %v", err)
	}
	if result.Status != state.StatusBlocked || result.Blocker == nil || result.Blocker.Comment == nil {
		t.Fatalf("published manifest = %#v", result)
	}
	if result.BlockerSequence != 1 || result.Blocker.ID != "blocker-1" || result.Blocker.Actor != manifest.Actor || result.Blocker.CreatedAt.IsZero() || result.Blocker.Marker == "" {
		t.Fatalf("blocker intent = %#v", result.Blocker)
	}
	if !strings.Contains(result.Blocker.Marker, manifest.WorkflowID) || !strings.Contains(result.Blocker.Marker, result.Blocker.ID) {
		t.Fatalf("marker %q does not contain workflow and blocker identities", result.Blocker.Marker)
	}
	for _, secret := range []string{"super-secret-value", "cloud-secret", "ghp_abcdefghijklmnopqrstuvwxyz123456", "sk-abcdefghijklmnopqrstuvwxyz"} {
		if strings.Contains(result.Blocker.Question, secret) || strings.Contains(comments.postedBody, secret) {
			t.Fatalf("credential %q escaped redaction: blocker=%q post=%q", secret, result.Blocker.Question, comments.postedBody)
		}
	}
	if !strings.Contains(comments.postedBody, result.Blocker.Question) || !strings.Contains(comments.postedBody, result.Blocker.Marker) {
		t.Fatalf("posted body %q does not contain persisted question and marker", comments.postedBody)
	}
	wantBody := "## 🤖 AWDev\n\nAWDev needs human input to continue the `spec` phase:\n\n" + result.Blocker.Question + "\n\n" + result.Blocker.Marker + "\n\n_This comment was generated automatically by AWDev_"
	if comments.postedBody != wantBody {
		t.Fatalf("posted body = %q, want %q", comments.postedBody, wantBody)
	}
	if len(states.saved) < 3 || states.saved[0].Blocker == nil || states.saved[0].Blocker.Comment != nil || states.saved[0].Status != state.StatusRunning {
		t.Fatalf("first durable state was not running blocker intent: %#v", states.saved)
	}
	if states.saved[len(states.saved)-1].Status != state.StatusBlocked {
		t.Fatalf("last durable state = %#v", states.saved[len(states.saved)-1])
	}
}

func TestRepeatedBlockerAdvancesItsDurableIdentity(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = answeredBlocker(state.PhaseImplementation)
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{actor: manifest.Actor, createdAt: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)}
	service := workflow.NewBlockerService(states, states, comments, func() time.Time { return time.Date(2026, 9, 1, 12, 59, 0, 0, time.UTC) })

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{Phase: manifest.Phase, Question: "One more decision?"})
	if err != nil {
		t.Fatal(err)
	}
	if result.BlockerSequence != 2 || result.Blocker.ID != "blocker-2" || !strings.Contains(result.Blocker.Marker, "blocker-2") {
		t.Fatalf("repeated blocker = %#v", result.Blocker)
	}
}

func TestRepeatedBlockerRecordsTheCurrentGitHubPublisher(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = answeredBlocker(state.PhaseImplementation)
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{
		actor:     "awdev[bot]",
		createdAt: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC),
	}
	service := workflow.NewBlockerService(states, states, comments, func() time.Time {
		return time.Date(2026, 9, 1, 12, 59, 0, 0, time.UTC)
	})

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase: manifest.Phase, Question: "One more decision?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Blocker == nil || result.Blocker.Actor != "awdev[bot]" {
		t.Fatalf("blocker publisher = %#v", result.Blocker)
	}
}

func TestRepeatedBlockerDoesNotReconcileFutureMarkerEmbeddedInPriorQuestion(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = answeredBlocker(state.PhaseImplementation)
	manifest.Blocker.Question = "Do not confuse this text with " + state.BlockerMarker(manifest.WorkflowID, "blocker-2")
	comments := &fakeCommentGateway{
		actor:     manifest.Actor,
		createdAt: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC),
		comments:  []githubapi.IssueComment{blockerIssueComment(manifest)},
	}
	states := &blockerState{manifest: manifest}
	service := workflow.NewBlockerService(states, states, comments, func() time.Time {
		return time.Date(2026, 9, 1, 12, 59, 0, 0, time.UTC)
	})

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase: manifest.Phase, Question: "A genuinely new question?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if comments.posts != 1 || result.Blocker == nil || result.Blocker.Comment == nil || result.Blocker.Comment.ID != "101" {
		t.Fatalf("future marker caused false reconciliation: posts=%d blocker=%#v", comments.posts, result.Blocker)
	}
}

func TestRepeatedBlockerDoesNotReconcileMarkerLineOutsideTheGeneratedFooter(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = answeredBlocker(state.PhaseImplementation)
	futureMarker := state.BlockerMarker(manifest.WorkflowID, "blocker-2")
	manifest.Blocker.Question = "Untrusted text:\n" + futureMarker
	comments := &fakeCommentGateway{
		actor:     manifest.Actor,
		createdAt: time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC),
		comments:  []githubapi.IssueComment{blockerIssueComment(manifest)},
	}
	states := &blockerState{manifest: manifest}
	service := workflow.NewBlockerService(states, states, comments, time.Now)

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase: manifest.Phase, Question: "A genuinely new question?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if comments.posts != 1 || result.Blocker == nil || result.Blocker.Comment == nil || result.Blocker.Comment.ID != "101" {
		t.Fatalf("untrusted marker line caused false reconciliation: posts=%d blocker=%#v", comments.posts, result.Blocker)
	}
}

func TestBlockerPublicationTechnicalFailureBecomesFailed(t *testing.T) {
	manifest := blockerManifest(state.PhaseSpec, state.StatusRunning)
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{listErr: errors.New("GitHub unavailable")}
	service := workflow.NewBlockerService(states, states, comments, time.Now)

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{Phase: manifest.Phase, Question: "Which API?"})
	if err == nil || result.Status != state.StatusFailed || result.LastError == nil {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestBlockerPublicationReconcilesAPreviouslyPostedMarkerWithoutPostingTwice(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = &state.Blocker{
		ID: "blocker-1", Phase: manifest.Phase, Question: "Which API?", Actor: manifest.Actor,
		Marker:    "<!-- awdev:blocker workflow=" + manifest.WorkflowID + " id=blocker-1 -->",
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	states := &blockerState{manifest: manifest, failCommentSaveOnce: true}
	comments := &fakeCommentGateway{actor: manifest.Actor, createdAt: time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)}
	service := workflow.NewBlockerService(states, states, comments, time.Now)

	if _, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{Phase: manifest.Phase, Question: manifest.Blocker.Question}); err == nil {
		t.Fatal("simulated state write failure was ignored")
	}
	if comments.posts != 1 || states.manifest.Blocker.Comment != nil || states.manifest.Status != state.StatusRunning {
		t.Fatalf("after failed reconciliation: posts=%d manifest=%#v", comments.posts, states.manifest)
	}
	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{Phase: manifest.Phase, Question: manifest.Blocker.Question})
	if err != nil {
		t.Fatalf("retry publication: %v", err)
	}
	if comments.posts != 1 || result.Status != state.StatusBlocked || result.Blocker.Comment.ID != "101" {
		t.Fatalf("retry duplicated or failed reconciliation: posts=%d manifest=%#v", comments.posts, result)
	}
}

func TestPendingBlockerUpdatesPublisherBeforePostingAfterCredentialSwitch(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = &state.Blocker{
		ID: "blocker-1", Phase: manifest.Phase, Question: "Which API?", Actor: manifest.Actor,
		Marker: state.BlockerMarker(manifest.WorkflowID, "blocker-1"), CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{actor: "awdev[bot]", createdAt: time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)}
	service := workflow.NewBlockerService(states, states, comments, time.Now)

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase: manifest.Phase, Question: manifest.Blocker.Question,
	})
	if err != nil {
		t.Fatal(err)
	}
	if comments.posts != 1 || result.Blocker == nil || result.Blocker.Actor != "awdev[bot]" || result.Blocker.Comment == nil {
		t.Fatalf("credential switch was not persisted before publication: posts=%d blocker=%#v", comments.posts, result.Blocker)
	}
	if len(states.saved) < 3 || states.saved[0].Blocker.Actor != "awdev[bot]" || states.saved[0].Blocker.Comment != nil {
		t.Fatalf("publisher update was not the first durable write: %#v", states.saved)
	}
}

func TestPendingBlockerPreservesUpdatedPublisherWhenPostingFails(t *testing.T) {
	manifest := blockerManifest(state.PhaseImplementation, state.StatusRunning)
	manifest.BlockerSequence = 1
	manifest.Blocker = &state.Blocker{
		ID: "blocker-1", Phase: manifest.Phase, Question: "Which API?", Actor: manifest.Actor,
		Marker: state.BlockerMarker(manifest.WorkflowID, "blocker-1"), CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	states := &blockerState{manifest: manifest}
	comments := &fakeCommentGateway{actor: "awdev[bot]", postErr: errors.New("GitHub unavailable")}
	service := workflow.NewBlockerService(states, states, comments, time.Now)

	result, err := service.Publish(context.Background(), "/repo", manifest.WorkflowID, workflow.BlockerRequest{
		Phase: manifest.Phase, Question: manifest.Blocker.Question,
	})
	if err == nil || result.Status != state.StatusFailed || result.Blocker == nil || result.Blocker.Actor != "awdev[bot]" {
		t.Fatalf("posting failure lost updated publisher: result=%#v error=%v", result, err)
	}
}

type blockerState struct {
	manifest            state.Manifest
	saved               []state.Manifest
	failCommentSaveOnce bool
}

func (store *blockerState) Read(_ string, workflowID string) (state.Manifest, error) {
	if store.manifest.WorkflowID != workflowID {
		return state.Manifest{}, errors.New("not found")
	}
	return store.manifest, nil
}

func (store *blockerState) Transition(_ string, workflowID string, next state.Manifest) error {
	if workflowID != store.manifest.WorkflowID {
		return errors.New("wrong workflow")
	}
	if store.failCommentSaveOnce && next.Blocker != nil && next.Blocker.Comment != nil && store.manifest.Blocker.Comment == nil {
		store.failCommentSaveOnce = false
		return errors.New("disk full")
	}
	store.manifest = next
	store.saved = append(store.saved, next)
	return nil
}

type fakeCommentGateway struct {
	comments   []githubapi.IssueComment
	postedBody string
	posts      int
	actor      string
	createdAt  time.Time
	listErr    error
	postErr    error
}

func (gateway *fakeCommentGateway) AuthenticatedActor(context.Context, string) (githubapi.Actor, error) {
	if gateway.listErr != nil {
		return githubapi.Actor{}, gateway.listErr
	}
	return githubapi.Actor{Login: gateway.actor}, nil
}

func (gateway *fakeCommentGateway) ListIssueComments(context.Context, string, string, int) ([]githubapi.IssueComment, error) {
	return append([]githubapi.IssueComment(nil), gateway.comments...), gateway.listErr
}

func (gateway *fakeCommentGateway) PostIssueComment(_ context.Context, _ string, _ string, _ int, body string) error {
	gateway.posts++
	gateway.postedBody = body
	if gateway.postErr != nil {
		return gateway.postErr
	}
	gateway.comments = append(gateway.comments, githubapi.IssueComment{
		ID: "101", URL: "https://github.com/owner/repository/issues/17#issuecomment-101",
		Author: githubapi.CommentAuthor{Login: gateway.actor, Type: "User"}, Body: body, CreatedAt: gateway.createdAt,
	})
	return nil
}

func blockerManifest(phase state.Phase, status state.Status) state.Manifest {
	return state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    fixedWorkflowID,
		Source:        state.SourceGitHub,
		Repository:    "owner/repository",
		DefaultBranch: "main",
		Issue: state.IssueSnapshot{
			Number: 17, Title: "A title", Body: "body", URL: "https://github.com/owner/repository/issues/17", State: "OPEN",
			UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		},
		Actor: "octocat", Phase: phase, Status: status, Branch: "gh-17-a-title",
		BaseSHA: strings.Repeat("a", 40), Worktree: ".awdev/worktrees/gh-17-a-title",
		SpecificationPath: ".awdev/specs/gh-17-a-title.md",
	}
}
