package workflow_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestStatusGitHubReadsManifestWithoutMutation(t *testing.T) {
	root := t.TempDir()
	manifest := statusManifest(root)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	manifestPath, err := state.ManifestPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeIssueUpdateChecker{updatedAt: manifest.Issue.UpdatedAt}
	service := workflow.NewStatusService(state.NewManifestReader(), checker)

	result, err := service.StatusGitHub(context.Background(), root, 17, workflow.StatusOptions{CheckIssue: true})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("status mutated the manifest")
	}
	if result.Manifest != manifest || result.Warning != "" {
		t.Fatalf("result = %#v", result)
	}
	if checker.repository != manifest.Repository || checker.issueNumber != 17 {
		t.Fatalf("checker inputs = %q #%d", checker.repository, checker.issueNumber)
	}
}

func TestStatusGitHubWarnsOnlyWhenLiveIssueIsNewer(t *testing.T) {
	root := t.TempDir()
	manifest := statusManifest(root)
	store := state.NewStore()
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		updatedAt time.Time
		wantWarn  bool
	}{
		{name: "newer", updatedAt: manifest.Issue.UpdatedAt.Add(time.Second), wantWarn: true},
		{name: "equal", updatedAt: manifest.Issue.UpdatedAt},
		{name: "older", updatedAt: manifest.Issue.UpdatedAt.Add(-time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := workflow.NewStatusService(state.NewManifestReader(), &fakeIssueUpdateChecker{updatedAt: test.updatedAt})
			result, err := service.StatusGitHub(context.Background(), root, 17, workflow.StatusOptions{CheckIssue: true})
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if (result.Warning != "") != test.wantWarn {
				t.Fatalf("warning = %q, want warning %v", result.Warning, test.wantWarn)
			}
			if result.Manifest.Issue != manifest.Issue {
				t.Fatal("live lookup replaced the saved issue snapshot")
			}
		})
	}
}

func TestStatusGitHubReportsMissingStateAndIssueCheckErrors(t *testing.T) {
	root := t.TempDir()
	store := state.NewStore()
	service := workflow.NewStatusService(state.NewManifestReader(), &fakeIssueUpdateChecker{})
	if _, err := service.StatusGitHub(context.Background(), root, 17, workflow.StatusOptions{}); err == nil {
		t.Fatal("missing workflow was accepted")
	}

	manifest := statusManifest(root)
	if err := store.Save(root, manifest); err != nil {
		t.Fatal(err)
	}
	service = workflow.NewStatusService(state.NewManifestReader(), &fakeIssueUpdateChecker{err: errors.New("malformed updatedAt")})
	if _, err := service.StatusGitHub(context.Background(), root, 17, workflow.StatusOptions{CheckIssue: true}); err == nil {
		t.Fatal("issue update lookup failure was ignored")
	}
}

type fakeIssueUpdateChecker struct {
	repository  string
	issueNumber int
	updatedAt   time.Time
	err         error
}

func (checker *fakeIssueUpdateChecker) IssueUpdatedAt(_ context.Context, _ string, repository string, issueNumber int) (time.Time, error) {
	checker.repository = repository
	checker.issueNumber = issueNumber
	return checker.updatedAt, checker.err
}

func statusManifest(root string) state.Manifest {
	return state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    fixedWorkflowID,
		Source:        state.SourceGitHub,
		Repository:    "owner/repository",
		DefaultBranch: "main",
		Issue:         state.IssueSnapshot{Number: 17, Title: "A title", Body: "saved body", URL: "https://github.com/owner/repository/issues/17", State: "OPEN", UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)},
		Actor:         "octocat",
		Phase:         state.PhaseInit,
		Status:        state.StatusRunning,
		Branch:        "gh-17-a-title",
		BaseSHA:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Worktree:      root + "/.awdev/worktrees/gh-17-a-title",
	}
}
