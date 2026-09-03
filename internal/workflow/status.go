package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

// StatusOptions controls optional read-only status checks.
type StatusOptions struct {
	CheckIssue bool
}

// StatusResult contains saved state plus an optional live-drift warning.
type StatusResult struct {
	Manifest state.Manifest
	Warning  string
}

// StatusReader reads a complete workflow manifest.
type StatusReader interface {
	ReadExisting(string, int) (state.ExistingWorkflow, error)
}

// IssueUpdateChecker reads only the live issue update timestamp.
type IssueUpdateChecker interface {
	IssueUpdatedAt(context.Context, string, string, int) (time.Time, error)
}

// StatusService provides mutation-free workflow inspection.
type StatusService struct {
	reader       StatusReader
	issueChecker IssueUpdateChecker
}

// NewStatusService constructs a status application service.
func NewStatusService(reader StatusReader, issueChecker IssueUpdateChecker) *StatusService {
	return &StatusService{reader: reader, issueChecker: issueChecker}
}

// StatusGitHub reads durable status and optionally detects live issue drift.
func (service *StatusService) StatusGitHub(ctx context.Context, controllerRoot string, issueNumber int, options StatusOptions) (StatusResult, error) {
	if issueNumber <= 0 {
		return StatusResult{}, errors.New("issue number must be positive")
	}
	if service == nil || service.reader == nil {
		return StatusResult{}, errors.New("status service is not configured")
	}
	existing, err := service.reader.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return StatusResult{}, fmt.Errorf("read workflow for GitHub issue #%d: %w", issueNumber, err)
	}
	if !existing.Exists || existing.Manifest == nil {
		return StatusResult{}, fmt.Errorf("workflow for GitHub issue #%d does not exist", issueNumber)
	}
	manifest := *existing.Manifest
	result := StatusResult{Manifest: manifest}
	if !options.CheckIssue {
		return result, nil
	}
	if service.issueChecker == nil {
		return StatusResult{}, errors.New("live issue checking is unavailable")
	}
	liveUpdatedAt, err := service.issueChecker.IssueUpdatedAt(ctx, controllerRoot, manifest.Repository, issueNumber)
	if err != nil {
		return StatusResult{}, fmt.Errorf("check live GitHub issue: %w", err)
	}
	if liveUpdatedAt.After(manifest.Issue.UpdatedAt) {
		result.Warning = fmt.Sprintf("GitHub issue %s#%d changed after this workflow started; continuing to use the saved snapshot.", manifest.Repository, issueNumber)
	}
	return result, nil
}
