package state

import (
	"errors"
	"fmt"
	"os"
)

// ExistingWorkflow is durable state used to short-circuit a duplicate run
// before GitHub or any later bootstrap collaborator is invoked.
type ExistingWorkflow struct {
	Exists   bool
	Manifest *Manifest
}

// ExistingReader checks for previously persisted workflow state.
type ExistingReader interface {
	ReadExisting(string, int) (ExistingWorkflow, error)
}

// ManifestReader strictly reads complete manifests for duplicate detection.
type ManifestReader struct {
	store *Store
}

// NewManifestReader constructs a complete manifest reader.
func NewManifestReader() ManifestReader { return ManifestReader{store: NewStore()} }

// ReadExisting returns Exists=false only when manifest.json is absent.
func (reader ManifestReader) ReadExisting(controllerRoot string, issueNumber int) (ExistingWorkflow, error) {
	if issueNumber <= 0 {
		return ExistingWorkflow{}, errors.New("issue number must be positive")
	}
	workflowIDs, err := reader.store.workflowIDs(controllerRoot)
	if err != nil {
		return ExistingWorkflow{}, fmt.Errorf("list workflow manifests: %w", err)
	}
	var existing ExistingWorkflow
	for _, workflowID := range workflowIDs {
		manifest, readErr := reader.store.Read(controllerRoot, workflowID)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return ExistingWorkflow{}, fmt.Errorf("read existing workflow manifest: %w", readErr)
		}
		if manifest.Source != SourceGitHub || manifest.Issue.Number != issueNumber {
			continue
		}
		if existing.Exists {
			return ExistingWorkflow{}, fmt.Errorf("multiple workflows exist for GitHub issue #%d", issueNumber)
		}
		existing = ExistingWorkflow{Exists: true, Manifest: &manifest}
	}
	return existing, nil
}
