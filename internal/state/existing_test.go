package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestManifestReaderRecognizesSupportedExistingStatuses(t *testing.T) {
	for _, status := range []string{"running", "blocked", "failed", "done"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			manifest := validManifest(root)
			manifest.Phase = state.PhaseImplementation
			manifest.Status = state.Status(status)
			switch manifest.Status {
			case state.StatusBlocked:
				manifest.BlockerSequence = 1
				manifest.Blocker = publishedBlocker(state.PhaseImplementation)
			case state.StatusFailed:
				manifest.LastError = &state.WorkflowError{Code: "technical_failure", Message: "failure"}
			case state.StatusDone:
				manifest.Phase = state.PhaseDone
				manifest.Review = &state.ReviewCounters{Attempt: 1, MaxAttempts: 3}
				manifest.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
			}
			if err := state.NewStore().Save(root, manifest); err != nil {
				t.Fatal(err)
			}
			existing, err := state.NewManifestReader().ReadExisting(root, 17)
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			if !existing.Exists || existing.Manifest == nil || string(existing.Manifest.Status) != status {
				t.Fatalf("existing = %#v", existing)
			}
		})
	}
}

func TestManifestReaderReportsAbsentAndMalformedManifests(t *testing.T) {
	root := t.TempDir()
	reader := state.NewManifestReader()
	if existing, err := reader.ReadExisting(root, 17); err != nil || existing.Exists {
		t.Fatalf("absent manifest = %#v, %v", existing, err)
	}
	manifest := validManifest(root)
	manifest.Status = "unknown"
	path, err := state.ManifestPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadExisting(root, 17); err == nil {
		t.Fatal("malformed manifest was accepted")
	}
}

func TestManifestReaderRejectsDuplicateWorkflowsForOneIssue(t *testing.T) {
	root := t.TempDir()
	first := validManifest(root)
	if err := state.NewStore().Save(root, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.WorkflowID = "wf_fedcba9876543210fedcba9876543210"
	if err := state.NewStore().Save(root, second); err != nil {
		t.Fatal(err)
	}
	if _, err := state.NewManifestReader().ReadExisting(root, 17); err == nil {
		t.Fatal("duplicate issue workflows were accepted")
	}
}
