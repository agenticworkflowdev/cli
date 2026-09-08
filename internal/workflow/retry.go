package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/agenticworkflowdev/cli/internal/state"
)

// RetryService holds the issue workflow lock while invoking the one supported
// terminal retry path.
type RetryService struct {
	locker    state.Locker
	existing  state.ExistingReader
	publisher PublicationRetrier
}

func NewRetryService(locker state.Locker, existing state.ExistingReader, publisher PublicationRetrier) *RetryService {
	return &RetryService{locker: locker, existing: existing, publisher: publisher}
}

func (service *RetryService) RetryGitHub(ctx context.Context, controllerRoot string, issueNumber int) (result PublicationResult, err error) {
	if issueNumber <= 0 {
		return PublicationResult{}, errors.New("issue number must be positive")
	}
	if service == nil || service.locker == nil || service.existing == nil || service.publisher == nil {
		return PublicationResult{}, errors.New("GitHub retry service is not fully configured")
	}
	issueKey, err := state.GitHubIssueKey(issueNumber)
	if err != nil {
		return PublicationResult{}, err
	}
	lock, err := service.locker.Acquire(ctx, controllerRoot, issueKey)
	if err != nil {
		return PublicationResult{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = releaseErr
			result = PublicationResult{}
		}
	}()

	existing, err := service.existing.ReadExisting(controllerRoot, issueNumber)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("read workflow for retry: %w", err)
	}
	if !existing.Exists || existing.Manifest == nil {
		return PublicationResult{}, fmt.Errorf("no workflow exists for GitHub issue #%d", issueNumber)
	}
	manifest := existing.Manifest
	if manifest.Phase != state.PhasePullRequest || manifest.Status != state.StatusFailed {
		return PublicationResult{}, fmt.Errorf("retry is supported only for pull_request/failed; got %s/%s; see the manual recovery guidance in docs/manual-recovery.md", manifest.Phase, manifest.Status)
	}
	return service.publisher.Retry(ctx, controllerRoot, manifest.WorkflowID)
}
