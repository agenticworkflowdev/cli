package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestRetryGitHubAcceptsOnlyFailedPullRequestAndHoldsWorkflowLock(t *testing.T) {
	events := []string{}
	manifest := state.Manifest{WorkflowID: fixedWorkflowID, Phase: state.PhasePullRequest, Status: state.StatusFailed}
	retrier := &fakePublicationRetrier{events: &events, result: workflow.PublicationResult{Manifest: state.Manifest{WorkflowID: fixedWorkflowID, Phase: state.PhaseDone, Status: state.StatusDone}}}
	service := workflow.NewRetryService(
		fakeLocker{events: &events, lock: &fakeLock{events: &events}},
		fakeExisting{events: &events, workflow: state.ExistingWorkflow{Exists: true, Manifest: &manifest}},
		retrier,
	)
	result, err := service.RetryGitHub(context.Background(), "/repo", 17)
	if err != nil || result.Manifest.Status != state.StatusDone {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if want := []string{"lock:gh-17", "state:17", "retry:" + fixedWorkflowID, "unlock"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRetryGitHubRejectsEveryOtherWorkflowState(t *testing.T) {
	for _, condition := range []struct {
		phase  state.Phase
		status state.Status
	}{{state.PhaseReview, state.StatusFailed}, {state.PhasePullRequest, state.StatusRunning}, {state.PhaseDone, state.StatusDone}} {
		t.Run(string(condition.phase)+"/"+string(condition.status), func(t *testing.T) {
			events := []string{}
			manifest := state.Manifest{WorkflowID: fixedWorkflowID, Phase: condition.phase, Status: condition.status}
			retrier := &fakePublicationRetrier{events: &events, err: errors.New("must not retry")}
			service := workflow.NewRetryService(
				fakeLocker{events: &events, lock: &fakeLock{events: &events}},
				fakeExisting{events: &events, workflow: state.ExistingWorkflow{Exists: true, Manifest: &manifest}}, retrier,
			)
			_, err := service.RetryGitHub(context.Background(), "/repo", 17)
			if err == nil || !strings.Contains(err.Error(), "manual recovery") || retrier.called {
				t.Fatalf("error = %v, retrier called = %t", err, retrier.called)
			}
		})
	}
}

type fakePublicationRetrier struct {
	events *[]string
	result workflow.PublicationResult
	err    error
	called bool
}

func (retrier *fakePublicationRetrier) Retry(_ context.Context, _ string, workflowID string) (workflow.PublicationResult, error) {
	retrier.called = true
	*retrier.events = append(*retrier.events, "retry:"+workflowID)
	return retrier.result, retrier.err
}
