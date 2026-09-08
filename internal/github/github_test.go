package github_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestClientFetchesStructuredSnapshotWithSafeArguments(t *testing.T) {
	body := "line one\n<!-- marker -->\n$() ; — Unicode"
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"release.LOCK"}}`},
		{stdout: `{"data":{"viewer":{"login":"octocat"}}}`},
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
		{"gh", "api", "graphql", "-f", "query=query { viewer { login } }"},
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

func TestClientFetchesGitHubAppInstallationActorThroughGraphQLViewer(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{stdout: `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"main"}}`},
		{stdout: `{"data":{"viewer":{"login":"awdev[bot]"}}}`},
		{stdout: issueJSON(17, "OPEN")},
	}}

	snapshot, err := githubapi.NewClient("gh", runner).Fetch(context.Background(), "/repo", 17)
	if err != nil {
		t.Fatalf("fetch with installation identity: %v", err)
	}
	if snapshot.Actor.Login != "awdev[bot]" {
		t.Fatalf("actor = %#v", snapshot.Actor)
	}
	wantActorArgv := []string{"gh", "api", "graphql", "-f", "query=query { viewer { login } }"}
	if !reflect.DeepEqual(runner.requests[1].Argv, wantActorArgv) {
		t.Fatalf("actor argv = %#v, want %#v", runner.requests[1].Argv, wantActorArgv)
	}
}

func TestClientRejectsInvalidResponses(t *testing.T) {
	validRepository := `{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"main"}}`
	validActor := `{"data":{"viewer":{"login":"octocat"}}}`
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
		{stdout: `{"data":{"viewer":{"login":"octocat"}}}`},
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

func TestClientReadsIssueUpdateTimestamp(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{stdout: `{"updatedAt":"2026-08-29T12:00:00Z"}`}}}
	client := githubapi.NewClient("gh", runner)
	got, err := client.IssueUpdatedAt(context.Background(), "/repo", "owner/repository", 17)
	if err != nil {
		t.Fatalf("read issue update: %v", err)
	}
	want := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("updatedAt = %s, want %s", got, want)
	}
	wantArgv := []string{"gh", "issue", "view", "17", "--repo", "owner/repository", "--json", "updatedAt"}
	if !reflect.DeepEqual(runner.requests[0].Argv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", runner.requests[0].Argv, wantArgv)
	}
}

func TestClientRejectsMalformedIssueUpdateTimestamp(t *testing.T) {
	for _, output := range []string{`{}`, `{"updatedAt":"bad"}`, `{"updatedAt":"2026-08-29T12:00:00Z","extra":true}`} {
		runner := &fakeRunner{responses: []fakeResponse{{stdout: output}}}
		if _, err := githubapi.NewClient("gh", runner).IssueUpdatedAt(context.Background(), "/repo", "owner/repository", 17); err == nil {
			t.Fatalf("malformed update response %q was accepted", output)
		}
	}
}

func TestClientListsEveryIssueCommentPage(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{stdout: `[[{"id":101,"html_url":"https://github.com/owner/repository/issues/17#issuecomment-101","user":{"login":"octocat","type":"User"},"body":"first","created_at":"2026-08-29T12:00:00Z"}],[{"id":102,"html_url":"https://github.com/owner/repository/issues/17#issuecomment-102","user":{"login":"dependabot[bot]","type":"Bot"},"body":"second","created_at":"2026-08-29T12:01:00Z"}]]`}}}
	comments, err := githubapi.NewClient("gh", runner).ListIssueComments(context.Background(), "/repo", "owner/repository", 17)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(comments) != 2 || comments[0].ID != "101" || comments[1].Author.Type != "Bot" {
		t.Fatalf("comments = %#v", comments)
	}
	want := []string{"gh", "api", "--paginate", "--slurp", "/repos/owner/repository/issues/17/comments?per_page=100"}
	if !reflect.DeepEqual(runner.requests[0].Argv, want) {
		t.Fatalf("argv = %#v, want %#v", runner.requests[0].Argv, want)
	}
}

func TestClientPostsIssueCommentBodyOnlyThroughStdin(t *testing.T) {
	body := "question with --flags $(command) and ; separators\n<!-- awdev:blocker -->"
	runner := &fakeRunner{responses: []fakeResponse{{stdout: "https://github.com/owner/repository/issues/17#issuecomment-103\n"}}}
	if err := githubapi.NewClient("gh", runner).PostIssueComment(context.Background(), "/repo", "owner/repository", 17, body); err != nil {
		t.Fatalf("post comment: %v", err)
	}
	want := []string{"gh", "issue", "comment", "17", "--repo", "owner/repository", "--body-file", "-"}
	if !reflect.DeepEqual(runner.requests[0].Argv, want) {
		t.Fatalf("argv = %#v, want %#v", runner.requests[0].Argv, want)
	}
	if got := string(runner.requests[0].Stdin); got != body {
		t.Fatalf("stdin = %q, want %q", got, body)
	}
	for _, argument := range runner.requests[0].Argv {
		if strings.Contains(argument, "question") || strings.Contains(argument, "$(command)") {
			t.Fatalf("untrusted body leaked into argv: %#v", runner.requests[0].Argv)
		}
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
