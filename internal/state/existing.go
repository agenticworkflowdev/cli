package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExistingWorkflow is the minimum durable state needed to short-circuit a
// duplicate run before GitHub or any later bootstrap collaborator is invoked.
type ExistingWorkflow struct {
	Exists bool
	Phase  Phase
	Status Status
}

// Phase names the durable workflow phase. Slice 4 defines its closed set.
type Phase string

// Status is the durable workflow execution condition.
type Status string

const (
	StatusRunning Status = "running"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
	StatusDone    Status = "done"
)

// ExistingReader checks for previously persisted workflow state.
type ExistingReader interface {
	ReadExisting(string, string) (ExistingWorkflow, error)
}

// ManifestReader reads the minimal public phase and status from a manifest.
// Full manifest decoding and transition validation belong to Slice 4.
type ManifestReader struct{}

// NewManifestReader constructs a minimal manifest reader.
func NewManifestReader() ManifestReader { return ManifestReader{} }

// ReadExisting returns Exists=false only when manifest.json is absent.
func (ManifestReader) ReadExisting(controllerRoot, workflowID string) (ExistingWorkflow, error) {
	if err := validateWorkflowID(workflowID); err != nil {
		return ExistingWorkflow{}, err
	}
	manifestPath := filepath.Join(controllerRoot, ".awdev", "issues", workflowID, "manifest.json")
	contents, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return ExistingWorkflow{}, nil
	}
	if err != nil {
		return ExistingWorkflow{}, fmt.Errorf("read existing workflow manifest: %w", err)
	}
	var raw struct {
		Phase  *Phase  `json:"phase"`
		Status *Status `json:"status"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&raw); err != nil {
		return ExistingWorkflow{}, fmt.Errorf("decode existing workflow manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ExistingWorkflow{}, errors.New("decode existing workflow manifest: expected one JSON object")
	}
	if raw.Phase == nil || *raw.Phase == "" || raw.Status == nil {
		return ExistingWorkflow{}, errors.New("existing workflow manifest is missing phase or status")
	}
	switch *raw.Status {
	case StatusRunning, StatusBlocked, StatusFailed, StatusDone:
	default:
		return ExistingWorkflow{}, fmt.Errorf("existing workflow manifest has invalid status %q", *raw.Status)
	}
	return ExistingWorkflow{Exists: true, Phase: *raw.Phase, Status: *raw.Status}, nil
}
