package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestStorePersistsReconAsControllerOwnedWorkflowArtifact(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	want := []byte("# Recon\n\n## Relevant architecture\n\n- workflow package\n")
	if err := store.SaveRecon(root, manifest.WorkflowID, want); err != nil {
		t.Fatalf("save recon: %v", err)
	}
	got, err := store.ReadRecon(root, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("read recon: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("recon = %q, want %q", got, want)
	}

	path, err := state.ReconPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, ".awdev", "issues", manifest.WorkflowID, "recon.md")
	if path != wantPath {
		t.Fatalf("recon path = %q, want %q", path, wantPath)
	}
}

func TestStoreRejectsEmptyOrNonRegularReconArtifact(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecon(root, manifest.WorkflowID, []byte(" \n\t")); err == nil {
		t.Fatal("empty recon was accepted")
	}

	reconPath, err := state.ReconPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# Outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, reconPath); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecon(root, manifest.WorkflowID, []byte("# Recon\n")); err == nil {
		t.Fatal("symlink recon path was accepted")
	}
}
