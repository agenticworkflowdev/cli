package state_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestManifestRoundTripPreservesIssueSnapshot(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	manifest.Issue.Body = "line one\n<!-- marker -->\nUnicode — {{literal}} $()"
	store := state.NewStore()

	if err := store.Save(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	got, err := store.Read(root, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if got.Issue != manifest.Issue {
		t.Fatalf("issue snapshot = %#v, want %#v", got.Issue, manifest.Issue)
	}
	if got != manifest {
		t.Fatalf("manifest round trip = %#v, want %#v", got, manifest)
	}
	contents, err := os.ReadFile(filepath.Join(root, ".awdev", "issues", manifest.WorkflowID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "specification_path") {
		t.Fatalf("initial manifest unexpectedly contains a specification path: %s", contents)
	}
}

func TestNewWorkflowIDReturnsOpaqueRandomIdentity(t *testing.T) {
	first, err := state.NewWorkflowID()
	if err != nil {
		t.Fatalf("generate first workflow ID: %v", err)
	}
	second, err := state.NewWorkflowID()
	if err != nil {
		t.Fatalf("generate second workflow ID: %v", err)
	}
	if len(first) != 35 || !strings.HasPrefix(first, "wf_") || strings.Trim(first[3:], "0123456789abcdef") != "" {
		t.Fatalf("workflow ID = %q, want wf_ followed by 32 lowercase hex characters", first)
	}
	if second == first {
		t.Fatalf("consecutive workflow IDs matched: %q", first)
	}
}

func TestManifestValidationRejectsInvalidFieldsAndCombinations(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		edit func(*state.Manifest)
		want string
	}{
		{name: "schema", edit: func(value *state.Manifest) { value.SchemaVersion = 2 }, want: "schema_version"},
		{name: "workflow identity", edit: func(value *state.Manifest) { value.WorkflowID = "other-17" }, want: "workflow identity"},
		{name: "issue identity", edit: func(value *state.Manifest) { value.Issue.Number = 18 }, want: "issue URL"},
		{name: "source", edit: func(value *state.Manifest) { value.Source = "linear" }, want: "source"},
		{name: "repository", edit: func(value *state.Manifest) { value.Repository = "bad" }, want: "repository"},
		{name: "issue URL", edit: func(value *state.Manifest) { value.Issue.URL = "https://example.com/17" }, want: "issue URL"},
		{name: "issue state", edit: func(value *state.Manifest) { value.Issue.State = "CLOSED" }, want: "issue state"},
		{name: "absolute worktree", edit: func(value *state.Manifest) { value.Worktree = "relative" }, want: "worktree"},
		{name: "worktree shape", edit: func(value *state.Manifest) { value.Worktree = filepath.Join(root, "outside", "gh-17-title") }, want: "worktree"},
		{name: "spec path", edit: func(value *state.Manifest) { value.SpecificationPath = "../escape.md" }, want: "specification"},
		{name: "base sha", edit: func(value *state.Manifest) { value.BaseSHA = "abc" }, want: "base SHA"},
		{name: "phase", edit: func(value *state.Manifest) { value.Phase = "unknown" }, want: "phase"},
		{name: "status", edit: func(value *state.Manifest) { value.Status = "unknown" }, want: "status"},
		{name: "blocked combination", edit: func(value *state.Manifest) { value.Phase = state.PhaseSpec; value.Status = state.StatusBlocked }, want: "blocker"},
		{name: "failed combination", edit: func(value *state.Manifest) { value.Phase = state.PhaseSpec; value.Status = state.StatusFailed }, want: "last_error"},
		{name: "done combination", edit: func(value *state.Manifest) {
			value.Phase = state.PhaseDone
			value.Status = state.StatusDone
			value.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
		}, want: "pull request"},
		{name: "review count", edit: func(value *state.Manifest) { value.Review = &state.ReviewCounters{Attempt: 4, MaxAttempts: 3} }, want: "review"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest(root)
			test.edit(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestManifestValidationAcceptsEverySupportedPhaseStatusCombination(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		phase  state.Phase
		status state.Status
	}{
		{state.PhaseInit, state.StatusRunning},
		{state.PhaseSpec, state.StatusRunning},
		{state.PhaseSpec, state.StatusBlocked},
		{state.PhaseSpec, state.StatusFailed},
		{state.PhaseImplementation, state.StatusRunning},
		{state.PhaseImplementation, state.StatusBlocked},
		{state.PhaseImplementation, state.StatusFailed},
		{state.PhaseReview, state.StatusRunning},
		{state.PhaseReview, state.StatusBlocked},
		{state.PhaseReview, state.StatusFailed},
		{state.PhasePullRequest, state.StatusRunning},
		{state.PhasePullRequest, state.StatusFailed},
		{state.PhaseDone, state.StatusDone},
	}

	for _, test := range tests {
		t.Run(string(test.phase)+"_"+string(test.status), func(t *testing.T) {
			manifest := validManifest(root)
			manifest.Phase = test.phase
			manifest.Status = test.status
			if test.phase == state.PhaseReview || test.phase == state.PhasePullRequest || test.phase == state.PhaseDone {
				manifest.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
			}
			if test.status == state.StatusBlocked {
				manifest.Blocker = publishedBlocker(test.phase)
			}
			if test.status == state.StatusFailed {
				manifest.LastError = &state.WorkflowError{Code: "technical_failure", Message: "safe diagnostic"}
			}
			if test.status == state.StatusDone {
				manifest.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
			}
			if err := manifest.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func publishedBlocker(phase state.Phase) *state.Blocker {
	return &state.Blocker{
		ID:       "blocker-1",
		Phase:    phase,
		Question: "Which behavior should be used?",
		Comment:  &state.SourceReference{ID: "123", URL: "https://github.com/owner/repository/issues/17#issuecomment-123"},
	}
}

func TestStoreStrictlyRejectsUnknownManifestFields(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	if err := state.NewStore().Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	manifestPath, err := state.ManifestPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "{", `{"unexpected":true,`, 1))
	if err := os.WriteFile(manifestPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.NewStore().Read(root, manifest.WorkflowID); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
}

func validManifest(root string) state.Manifest {
	return state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    "wf_0123456789abcdef0123456789abcdef",
		Source:        state.SourceGitHub,
		Repository:    "owner/repository",
		DefaultBranch: "main",
		Issue: state.IssueSnapshot{
			Number:    17,
			Title:     "A title",
			Body:      "body",
			URL:       "https://github.com/owner/repository/issues/17",
			State:     "OPEN",
			UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		},
		Actor:    "octocat",
		Phase:    state.PhaseInit,
		Status:   state.StatusRunning,
		Branch:   "gh-17-a-title",
		BaseSHA:  strings.Repeat("a", 40),
		Worktree: filepath.Join(root, ".awdev", "worktrees", "gh-17-a-title"),
	}
}
