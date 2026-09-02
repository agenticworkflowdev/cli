package workflow_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestWorktreeBootstrapperCarriesValidatedGitHubData(t *testing.T) {
	preparer := &fakeWorktreePreparer{result: gitrepo.Worktree{Branch: "gh-17-a-title"}}
	bootstrapper := workflow.NewWorktreeBootstrapper(preparer)
	bootstrap := workflow.Bootstrap{ControllerRoot: "/repo", WorkflowID: "gh-17", Snapshot: validSnapshot()}

	result, err := bootstrapper.Continue(context.Background(), bootstrap)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if result != preparer.result {
		t.Fatalf("result = %#v, want %#v", result, preparer.result)
	}
	want := gitrepo.WorktreeRequest{
		ControllerRoot:     "/repo",
		RepositoryIdentity: "owner/repository",
		DefaultBranch:      "main",
		WorkflowID:         "gh-17",
		IssueNumber:        17,
		IssueTitle:         "A title",
	}
	if !reflect.DeepEqual(preparer.request, want) {
		t.Fatalf("request = %#v, want %#v", preparer.request, want)
	}
}

type fakeWorktreePreparer struct {
	request gitrepo.WorktreeRequest
	result  gitrepo.Worktree
}

func (preparer *fakeWorktreePreparer) Prepare(_ context.Context, request gitrepo.WorktreeRequest) (gitrepo.Worktree, error) {
	preparer.request = request
	return preparer.result, nil
}
