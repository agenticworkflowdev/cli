package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestManifestReaderRecognizesSupportedExistingStatuses(t *testing.T) {
	for _, status := range []string{"running", "blocked", "failed", "done"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, ".awdev", "issues", "gh-17")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			contents := []byte(`{"schema_version":1,"phase":"implementation","status":"` + status + `"}`)
			if err := os.WriteFile(filepath.Join(directory, "manifest.json"), contents, 0o644); err != nil {
				t.Fatal(err)
			}
			existing, err := state.NewManifestReader().ReadExisting(root, "gh-17")
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			if !existing.Exists || string(existing.Status) != status || existing.Phase != "implementation" {
				t.Fatalf("existing = %#v", existing)
			}
		})
	}
}

func TestManifestReaderReportsAbsentAndMalformedManifests(t *testing.T) {
	root := t.TempDir()
	reader := state.NewManifestReader()
	if existing, err := reader.ReadExisting(root, "gh-17"); err != nil || existing.Exists {
		t.Fatalf("absent manifest = %#v, %v", existing, err)
	}
	directory := filepath.Join(root, ".awdev", "issues", "gh-17")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte(`{"phase":"init","status":"unknown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadExisting(root, "gh-17"); err == nil {
		t.Fatal("malformed manifest was accepted")
	}
}
