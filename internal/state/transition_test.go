package state_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestTransitionServiceEnumeratesAllowedTransitions(t *testing.T) {
	type point struct {
		phase  state.Phase
		status state.Status
	}
	points := []point{
		{state.PhaseInit, state.StatusRunning},
		{state.PhaseSpec, state.StatusRunning}, {state.PhaseSpec, state.StatusBlocked}, {state.PhaseSpec, state.StatusFailed},
		{state.PhaseImplementation, state.StatusRunning}, {state.PhaseImplementation, state.StatusBlocked}, {state.PhaseImplementation, state.StatusFailed},
		{state.PhaseReview, state.StatusRunning}, {state.PhaseReview, state.StatusBlocked}, {state.PhaseReview, state.StatusFailed},
		{state.PhasePullRequest, state.StatusRunning}, {state.PhasePullRequest, state.StatusFailed},
		{state.PhaseDone, state.StatusDone},
	}
	allowed := map[string]bool{
		"init/running->init/running":                     true,
		"init/running->spec/running":                     true,
		"spec/running->spec/running":                     true,
		"spec/running->spec/blocked":                     true,
		"spec/running->spec/failed":                      true,
		"spec/running->implementation/running":           true,
		"spec/blocked->spec/blocked":                     true,
		"spec/blocked->spec/running":                     true,
		"spec/blocked->spec/failed":                      true,
		"spec/failed->spec/failed":                       true,
		"implementation/running->implementation/running": true,
		"implementation/running->implementation/blocked": true,
		"implementation/running->implementation/failed":  true,
		"implementation/running->review/running":         true,
		"implementation/blocked->implementation/blocked": true,
		"implementation/blocked->implementation/running": true,
		"implementation/blocked->implementation/failed":  true,
		"implementation/failed->implementation/failed":   true,
		"review/running->review/running":                 true,
		"review/running->review/blocked":                 true,
		"review/running->review/failed":                  true,
		"review/running->implementation/running":         true,
		"review/running->pull_request/running":           true,
		"review/blocked->review/blocked":                 true,
		"review/blocked->review/running":                 true,
		"review/blocked->review/failed":                  true,
		"review/failed->review/failed":                   true,
		"pull_request/running->pull_request/running":     true,
		"pull_request/running->pull_request/failed":      true,
		"pull_request/running->done/done":                true,
		"pull_request/failed->pull_request/failed":       true,
		"pull_request/failed->pull_request/running":      true,
	}

	for _, from := range points {
		for _, to := range points {
			name := fmt.Sprintf("%s/%s->%s/%s", from.phase, from.status, to.phase, to.status)
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				current := manifestAt(root, from.phase, from.status)
				next := manifestAt(root, to.phase, to.status)
				prepareTransitionData(&current, &next)
				store := state.NewStore()
				if err := store.Save(root, current); err != nil {
					t.Fatalf("save current: %v", err)
				}
				err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next)
				if allowed[name] && err != nil {
					t.Fatalf("allowed transition failed: %v", err)
				}
				if !allowed[name] && err == nil {
					t.Fatal("disallowed transition succeeded")
				}
				got, readErr := store.Read(root, current.WorkflowID)
				if readErr != nil {
					t.Fatalf("read result: %v", readErr)
				}
				if !allowed[name] && (got.Phase != current.Phase || got.Status != current.Status) {
					t.Fatalf("rejected transition persisted %s/%s", got.Phase, got.Status)
				}
			})
		}
	}
}

func TestTransitionAllowsInitToImplementationOnlyWhenSpecificationWasSkipped(t *testing.T) {
	root := t.TempDir()
	current := manifestAt(root, state.PhaseInit, state.StatusRunning)
	current.SkipSpecification = true
	next := current
	next.Phase = state.PhaseImplementation
	store := state.NewStore()
	if err := store.Save(root, current); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err != nil {
		t.Fatalf("transition skipped specification: %v", err)
	}
}

func TestTransitionRequiresBlockerIntentAnswerAndPullRequestPersistence(t *testing.T) {
	t.Run("block and resume", func(t *testing.T) {
		root := t.TempDir()
		store := state.NewStore()
		current := manifestAt(root, state.PhaseSpec, state.StatusRunning)
		if err := store.Save(root, current); err != nil {
			t.Fatal(err)
		}
		blocked := manifestAt(root, state.PhaseSpec, state.StatusBlocked)
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, blocked); err == nil {
			t.Fatal("workflow blocked without durable blocker intent")
		}
		intent := current
		intent.BlockerSequence = 1
		intent.Blocker = publishedBlocker(state.PhaseSpec)
		intent.Blocker.Question = "question"
		intent.Blocker.Comment = nil
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, intent); err != nil {
			t.Fatalf("persist intent: %v", err)
		}
		blocked.Blocker.Question = intent.Blocker.Question
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, blocked); err != nil {
			t.Fatalf("block after intent: %v", err)
		}
		resumed := blocked
		resumed.Status = state.StatusRunning
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, resumed); err == nil {
			t.Fatal("blocked workflow resumed without an answer")
		}
		resumed.Blocker.Answer = &state.BlockerAnswer{
			ID: "456", URL: "https://github.com/owner/repository/issues/17#issuecomment-456", Body: "Use option A", Author: "human", CreatedAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
		}
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, resumed); err != nil {
			t.Fatalf("resume with answer: %v", err)
		}
	})

	t.Run("pull request completion", func(t *testing.T) {
		root := t.TempDir()
		store := state.NewStore()
		current := manifestAt(root, state.PhasePullRequest, state.StatusRunning)
		if err := store.Save(root, current); err != nil {
			t.Fatal(err)
		}
		done := manifestAt(root, state.PhaseDone, state.StatusDone)
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, done); err == nil {
			t.Fatal("workflow completed before pull request identity was persisted")
		}
		withTree := current
		withTree.CommitTreeSHA = strings.Repeat("c", 40)
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, withTree); err != nil {
			t.Fatalf("persist controller commit tree: %v", err)
		}
		withCommit := withTree
		withCommit.CommitSHA = strings.Repeat("b", 40)
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, withCommit); err != nil {
			t.Fatalf("persist controller commit: %v", err)
		}
		withPullRequest := withCommit
		withPullRequest.PullRequest = done.PullRequest
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, withPullRequest); err != nil {
			t.Fatalf("persist pull request: %v", err)
		}
		done.CommitSHA = withCommit.CommitSHA
		done.CommitTreeSHA = withCommit.CommitTreeSHA
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, done); err != nil {
			t.Fatalf("complete after pull request persistence: %v", err)
		}
	})
}

func TestTransitionAllowsOnlyThePublisherToChangeBeforeBlockerPublication(t *testing.T) {
	root := t.TempDir()
	store := state.NewStore()
	current := manifestAt(root, state.PhaseImplementation, state.StatusRunning)
	current.BlockerSequence = 1
	current.Blocker = publishedBlocker(current.Phase)
	current.Blocker.Comment = nil
	if err := store.Save(root, current); err != nil {
		t.Fatal(err)
	}

	updated := current
	updated.Blocker = publishedBlocker(current.Phase)
	updated.Blocker.Comment = nil
	updated.Blocker.Actor = "awdev[bot]"
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, updated); err != nil {
		t.Fatalf("update pending blocker publisher: %v", err)
	}

	replaced := updated
	replaced.Blocker = publishedBlocker(current.Phase)
	replaced.Blocker.Comment = nil
	replaced.Blocker.Actor = updated.Blocker.Actor
	replaced.Blocker.Question = "replacement question"
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, replaced); err == nil {
		t.Fatal("pending blocker question was replaced with its publisher")
	}
}

func TestTransitionRequiresDurableIntentBeforeExternalIdentities(t *testing.T) {
	t.Run("blocker comment", func(t *testing.T) {
		root := t.TempDir()
		store := state.NewStore()
		current := manifestAt(root, state.PhaseSpec, state.StatusRunning)
		if err := store.Save(root, current); err != nil {
			t.Fatal(err)
		}
		next := current
		next.Blocker = publishedBlocker(state.PhaseSpec)
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err == nil {
			t.Fatal("external blocker comment identity was persisted without prior intent")
		}
	})

	t.Run("pull request", func(t *testing.T) {
		root := t.TempDir()
		store := state.NewStore()
		current := manifestAt(root, state.PhaseReview, state.StatusRunning)
		if err := store.Save(root, current); err != nil {
			t.Fatal(err)
		}
		next := manifestAt(root, state.PhasePullRequest, state.StatusRunning)
		next.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err == nil {
			t.Fatal("external pull request identity was persisted with the phase transition")
		}
	})

	t.Run("review counter pre-seeding", func(t *testing.T) {
		root := t.TempDir()
		store := state.NewStore()
		current := manifestAt(root, state.PhaseSpec, state.StatusRunning)
		if err := store.Save(root, current); err != nil {
			t.Fatal(err)
		}
		next := current
		next.Review = &state.ReviewCounters{Attempt: 0, MaxAttempts: 3}
		if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err == nil {
			t.Fatal("review counters were introduced before the review transition")
		}
	})
}

func TestTransitionRejectsSnapshotMutationBeforePersistence(t *testing.T) {
	root := t.TempDir()
	store := state.NewStore()
	current := manifestAt(root, state.PhaseInit, state.StatusRunning)
	if err := store.Save(root, current); err != nil {
		t.Fatal(err)
	}
	next := current
	next.Phase = state.PhaseSpec
	next.Issue.Body = "edited live body"
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err == nil {
		t.Fatal("transition changed the saved issue snapshot")
	}
	got, err := store.Read(root, current.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Issue.Body != current.Issue.Body || got.Phase != current.Phase {
		t.Fatalf("rejected transition persisted %#v", got)
	}
}

func TestTransitionAllowsSpecificationPathToBeRecordedOnce(t *testing.T) {
	root := t.TempDir()
	store := state.NewStore()
	current := manifestAt(root, state.PhaseSpec, state.StatusRunning)
	if err := store.Save(root, current); err != nil {
		t.Fatal(err)
	}
	next := current
	next.SpecificationPath = ".awdev/specs/" + current.Branch + ".md"
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err == nil {
		t.Fatal("recorded specification path before the specification existed")
	}
	worktree := absoluteManifestWorktree(t, root, current)
	if err := os.MkdirAll(filepath.Join(worktree, ".awdev", "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, filepath.FromSlash(next.SpecificationPath)), []byte("# Specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, next); err != nil {
		t.Fatalf("record specification path: %v", err)
	}
	changed := next
	changed.SpecificationPath = ".awdev/specs/other.md"
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, changed); err == nil {
		t.Fatal("recorded specification path was changed")
	}
}

func TestTransitionRecordsEachAgentSessionOnce(t *testing.T) {
	root := t.TempDir()
	store := state.NewStore()
	current := manifestAt(root, state.PhaseSpec, state.StatusRunning)
	if err := store.Save(root, current); err != nil {
		t.Fatal(err)
	}

	withSpecificationSession := current
	withSpecificationSession.AgentSessions = &state.AgentSessions{Provider: "codex", Specification: "spec-session"}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, withSpecificationSession); err != nil {
		t.Fatalf("record specification session: %v", err)
	}

	changed := withSpecificationSession
	changed.AgentSessions = &state.AgentSessions{Provider: "codex", Specification: "different-session"}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, changed); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("changed specification session error = %v", err)
	}

	changedProvider := withSpecificationSession
	changedProvider.AgentSessions = &state.AgentSessions{Provider: "claude-code", Specification: "spec-session"}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, changedProvider); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("changed agent session provider error = %v", err)
	}

	implementation := withSpecificationSession
	implementation.Phase = state.PhaseImplementation
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, implementation); err != nil {
		t.Fatalf("enter implementation: %v", err)
	}
	withImplementationSession := implementation
	withImplementationSession.AgentSessions = &state.AgentSessions{Provider: "codex", Specification: "spec-session", Implementation: "implementation-session"}
	if err := state.NewTransitionService(store).Transition(root, current.WorkflowID, withImplementationSession); err != nil {
		t.Fatalf("record implementation session: %v", err)
	}
}

func manifestAt(root string, phase state.Phase, status state.Status) state.Manifest {
	manifest := validManifest(root)
	manifest.Phase = phase
	manifest.Status = status
	if phase == state.PhaseReview || phase == state.PhasePullRequest || phase == state.PhaseDone {
		manifest.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
	}
	if status == state.StatusBlocked {
		manifest.BlockerSequence = 1
		manifest.Blocker = publishedBlocker(phase)
	}
	if status == state.StatusFailed {
		manifest.LastError = &state.WorkflowError{Code: "technical_failure", Message: "safe error"}
	}
	if status == state.StatusDone {
		manifest.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
	}
	return manifest
}

func prepareTransitionData(current, next *state.Manifest) {
	if next.Status == state.StatusBlocked && current.Status == state.StatusRunning && current.Phase == next.Phase {
		current.BlockerSequence = 1
		current.Blocker = publishedBlocker(current.Phase)
		current.Blocker.Comment = nil
	}
	if current.Status == state.StatusBlocked && current.Phase == next.Phase {
		next.BlockerSequence = current.BlockerSequence
		next.Blocker = publishedBlocker(current.Phase)
		if next.Status == state.StatusRunning {
			next.Blocker.Answer = &state.BlockerAnswer{
				ID: "456", URL: "https://github.com/owner/repository/issues/17#issuecomment-456", Body: "answer", Author: "human", CreatedAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			}
		}
	}
	if current.Phase == state.PhaseReview {
		current.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
		if next.Phase == state.PhaseImplementation {
			next.Review = &state.ReviewCounters{Attempt: 2, MaxAttempts: 3}
		} else {
			next.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
		}
	}
	if current.Phase == state.PhaseImplementation && next.Phase == state.PhaseReview {
		next.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
	}
	if current.Phase == state.PhasePullRequest && next.Phase == state.PhaseDone {
		current.PullRequest = next.PullRequest
	}
}
