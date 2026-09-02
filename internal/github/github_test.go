package github_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestClientFetchesStructuredSnapshotWithSafeArguments(t *testing.T) {
	body := "line one\n<!-- marker -->\n$() ; — Unicode"
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"release.LOCK"}}`},
		{stdout: `{"login":"octocat"}`},
		{stdout: fmt.Sprintf(`{"number":17,"title":"--danger; $(command)","body":%q,"url":"https://github.example/owner/repository/issues/17","state":"OPEN","updatedAt":"2026-08-28T12:00:00Z"}`, body)},
	}}

	snapshot, err := githubapi.NewClient("gh", runner).Fetch(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snapshot.Issue.Body != body || snapshot.Issue.Title != "--danger; $(command)" {
		t.Fatalf("snapshot did not preserve untrusted issue data: %#v", snapshot)
	}
	if snapshot.Repository.DefaultBranch != "release.LOCK" {
		t.Fatalf("valid case-sensitive branch name was not preserved: %#v", snapshot.Repository)
	}
	wantArgv := [][]string{
		{"gh", "repo", "view", "--json", "nameWithOwner,defaultBranchRef"},
		{"gh", "api", "user", "--jq", "{login: .login}"},
		{"gh", "issue", "view", "17", "--repo", "owner/repository", "--json", "body,number,state,title,updatedAt,url"},
	}
	if !reflect.DeepEqual(runner.argv(), wantArgv) {
		t.Fatalf("argv = %#v, want %#v", runner.argv(), wantArgv)
	}
	for _, request := range runner.requests {
		if request.Directory != "/repo" || request.Environment["GH_PROMPT_DISABLED"] != "1" || request.StdoutLimit <= 0 || request.StderrLimit <= 0 {
			t.Fatalf("unsafe/incomplete process request: %#v", request)
		}
	}
}

func TestClientRejectsInvalidResponses(t *testing.T) {
	validRepository := `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"main"}}`
	validActor := `{"login":"octocat"}`
	tests := []struct {
		name      string
		responses []fakeResponse
		want      string
	}{
		{name: "malformed repository", responses: []fakeResponse{{stdout: `{`}}, want: "decode GitHub repository"},
		{name: "missing default branch", responses: []fakeResponse{{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":null}`}}, want: "default branch is missing"},
		{name: "empty default branch", responses: []fakeResponse{{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":""}}`}}, want: "default branch is missing"},
		{name: "invalid default branch", responses: []fakeResponse{{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"feature..bad~ref"}}`}}, want: "default branch is malformed"},
		{name: "missing actor", responses: []fakeResponse{{stdout: validRepository}, {stdout: `{}`}}, want: "missing login"},
		{name: "missing issue", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {err: errors.New("exit"), stderr: "not found"}}, want: "not found"},
		{name: "malformed issue", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {stdout: `not-json`}}, want: "decode GitHub issue"},
		{name: "missing issue field", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {stdout: `{"number":17}`}}, want: "missing required fields"},
		{name: "closed issue", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {stdout: issueJSON(17, "CLOSED")}}, want: "is closed"},
		{name: "wrong issue", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {stdout: issueJSON(18, "OPEN")}}, want: "returned issue #18"},
		{name: "mismatched issue URL", responses: []fakeResponse{{stdout: validRepository}, {stdout: validActor}, {stdout: `{"number":17,"title":"title","body":"","url":"https://github.com/other/repository/issues/17","state":"OPEN","updatedAt":"2026-08-28T12:00:00Z"}`}}, want: "URL does not match"},
		{name: "truncated output", responses: []fakeResponse{{stdout: validRepository, stdoutTruncated: true}}, want: "capture limit"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: test.responses}
			_, err := githubapi.NewClient("gh", runner).Fetch(context.Background(), "/repo", 17)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestClientAcceptsLargeIssueBodyWithinBound(t *testing.T) {
	body := strings.Repeat("large Unicode body —\n", 10_000)
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"main"}}`},
		{stdout: `{"login":"octocat"}`},
		{stdout: fmt.Sprintf(`{"number":17,"title":"title","body":%q,"url":"https://github.com/owner/repository/issues/17","state":"OPEN","updatedAt":"2026-08-28T12:00:00Z"}`, body)},
	}}
	snapshot, err := githubapi.NewClient("gh", runner).Fetch(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("fetch large issue: %v", err)
	}
	if snapshot.Issue.Body != body {
		t.Fatalf("large body length = %d, want %d", len(snapshot.Issue.Body), len(body))
	}
}

func issueJSON(number int, state string) string {
	return fmt.Sprintf(`{"number":%d,"title":"title","body":"","url":"https://github.com/owner/repository/issues/%d","state":%q,"updatedAt":"2026-08-28T12:00:00Z"}`, number, number, state)
}

type fakeResponse struct {
	stdout          string
	stderr          string
	stdoutTruncated bool
	err             error
}

type fakeRunner struct {
	requests  []processrun.Request
	responses []fakeResponse
}

func (runner *fakeRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	runner.requests = append(runner.requests, request)
	response := runner.responses[len(runner.requests)-1]
	return processrun.Result{Stdout: []byte(response.stdout), Stderr: []byte(response.stderr), StdoutTruncated: response.stdoutTruncated}, response.err
}

func (runner *fakeRunner) argv() [][]string {
	result := make([][]string, len(runner.requests))
	for index, request := range runner.requests {
		result[index] = request.Argv
	}
	return result
}
