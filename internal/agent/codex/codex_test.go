package codex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/agent/codex"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestRunnerInvokesCodexThroughTheVerifiedContract(t *testing.T) {
	worktree := t.TempDir()
	outputDirectory := t.TempDir()
	schemaPath := filepath.Join(t.TempDir(), "agent-result.schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$PWD/argv.txt"
printf '%s' "$PWD" > "$PWD/cwd.txt"
/bin/cat > "$PWD/stdin.txt"
/usr/bin/env | /usr/bin/sort > "$PWD/environment.txt"
previous=
for argument in "$@"; do
  if [ "$previous" = "--output-last-message" ]; then output="$argument"; fi
  previous="$argument"
done
printf '%s' '{"status":"completed","summary":"specification written","question":""}' > "$output"
printf '%s\n' \
  '{"type":"thread.started","thread_id":"thread-123"}' \
  '{"type":"turn.started"}' \
  '{"type":"future.event","payload":{"ignored":true}}' \
  '{"type":"turn.completed"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
	t.Setenv("CODEX_API_KEY", "test-api-key")
	t.Setenv("CODEX_ACCESS_TOKEN", "test-access-token")
	t.Setenv("OPENAI_API_KEY", "test-openai-api-key")
	t.Setenv("AWDEV_UNRELATED_SECRET", "must-not-leak")

	runner, err := codex.NewRunner(binary, processrun.NewRunner())
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}
	request := agent.Request{
		Worktree:        worktree,
		Prompt:          "literal {{issue}} prompt\n",
		OutputSchema:    schemaPath,
		OutputDirectory: outputDirectory,
		Access:          agent.AccessWorkspaceWrite,
	}
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("run codex: %v", err)
	}

	argv := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(worktree, "argv.txt"))), "\n")
	if len(argv) != 11 {
		t.Fatalf("argv = %#v", argv)
	}
	wantPrefix := []string{"exec", "--json", "--sandbox", "workspace-write", "--output-schema", schemaPath, "--cd", worktree, "--output-last-message"}
	if !reflect.DeepEqual(argv[:len(wantPrefix)], wantPrefix) || argv[len(argv)-1] != "-" {
		t.Fatalf("argv = %#v, want prefix %#v and stdin sentinel", argv, wantPrefix)
	}
	if filepath.Dir(argv[9]) != outputDirectory {
		t.Fatalf("final output target = %q, want beneath %q", argv[9], outputDirectory)
	}
	wantCWD, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(worktree, "cwd.txt")); got != wantCWD {
		t.Fatalf("cwd = %q, want %q", got, wantCWD)
	}
	if got := readFile(t, filepath.Join(worktree, "stdin.txt")); got != request.Prompt {
		t.Fatalf("stdin = %q, want prompt", got)
	}
	environment := readFile(t, filepath.Join(worktree, "environment.txt"))
	for _, want := range []string{"AWDEV_WORKER=1", "CODEX_API_KEY=test-api-key", "CODEX_ACCESS_TOKEN=test-access-token", "OPENAI_API_KEY=test-openai-api-key", "CODEX_HOME="} {
		if !strings.Contains(environment, want) {
			t.Errorf("environment %q does not contain %q", environment, want)
		}
	}
	if strings.Contains(environment, "AWDEV_UNRELATED_SECRET") {
		t.Fatalf("unrelated environment leaked to Codex: %q", environment)
	}
	if result.SessionID != "thread-123" || len(result.Progress) != 4 {
		t.Fatalf("run result = %#v", result)
	}
	if got := string(result.FinalOutput); got != `{"status":"completed","summary":"specification written","question":""}` {
		t.Fatalf("final output = %q", got)
	}
	if _, err := os.Stat(argv[9]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary final output was retained: %v", err)
	}
}

func TestRunnerResumesAnExistingCodexSession(t *testing.T) {
	process := &fakeProcessRunner{
		stdout:      []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-existing\"}\n{\"type\":\"turn.completed\"}\n"),
		finalOutput: []byte(readFixture(t, "completed.json")),
	}
	runner := mustRunner(t, process)
	request := validRequest(t, agent.AccessReadOnly)
	request.ResumeSessionID = "thread-existing"

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	want := []string{
		"codex", "exec", "--json", "--sandbox", "read-only", "--cd", request.Worktree,
		"--output-schema", request.OutputSchema, "--output-last-message", argumentAfter(process.request.Argv, "--output-last-message"),
		"resume", "thread-existing", "-",
	}
	if !reflect.DeepEqual(process.request.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", process.request.Argv, want)
	}
	if result.SessionID != request.ResumeSessionID {
		t.Fatalf("session id = %q, want %q", result.SessionID, request.ResumeSessionID)
	}
}

func TestRunnerParsesLargeAndUnknownJSONLEventsWithoutAScannerLimit(t *testing.T) {
	large := strings.Repeat("x", 256*1024)
	progressFixture := strings.Replace(readFixture(t, "oversized-unknown.jsonl"), "__LARGE_PAYLOAD__", large, 1)
	process := &fakeProcessRunner{stdout: []byte(progressFixture), finalOutput: []byte(readFixture(t, "completed.json"))}
	runner := mustRunner(t, process)
	result, err := runner.Run(context.Background(), validRequest(t, agent.AccessReadOnly))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.SessionID != "thread-large" || len(result.Progress) != 3 || result.Progress[1].Type != "future.event" {
		t.Fatalf("result = %#v", result)
	}
	if got := process.request.Argv[4]; got != "read-only" {
		t.Fatalf("sandbox = %q, want read-only", got)
	}
}

func TestRunnerStreamsReadableProgressEventsWhileTheProcessRuns(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret-value")
	stdout := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-live"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"Inspecting the project"}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","aggregated_output":"ok secret-value\n","exit_code":0}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Implementation complete"}}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"
	process := &fakeProcessRunner{stdout: []byte(stdout), finalOutput: []byte(readFixture(t, "completed.json"))}
	runner := mustRunner(t, process)
	var streamed []agent.ProgressEvent
	request := validRequest(t, agent.AccessWorkspaceWrite)
	request.Progress = func(event agent.ProgressEvent) { streamed = append(streamed, event) }

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	want := []agent.ProgressEvent{
		{Type: "item.completed", Kind: agent.ProgressReasoning, Message: "Inspecting the project"},
		{Type: "item.started", Kind: agent.ProgressCommand, Message: "go test ./..."},
		{Type: "item.completed", Kind: agent.ProgressCommandOutput, Message: "ok [REDACTED]\n", ExitCode: &zero},
		{Type: "item.completed", Kind: agent.ProgressMessage, Message: "Implementation complete"},
	}
	if !reflect.DeepEqual(streamed, want) {
		t.Fatalf("streamed progress = %#v, want %#v", streamed, want)
	}
	if got, want := result.Progress[3].Message, "ok [REDACTED]\n"; got != want {
		t.Fatalf("retained command output = %q, want %q", got, want)
	}
}

func TestRunnerRejectsMalformedProgressAndMissingFinalOutput(t *testing.T) {
	tests := []struct {
		name        string
		stdout      string
		finalOutput []byte
		want        string
	}{
		{name: "malformed event", stdout: readFixture(t, "malformed.jsonl"), finalOutput: []byte(readFixture(t, "completed.json")), want: "JSONL"},
		{name: "missing event type", stdout: readFixture(t, "missing-type.jsonl"), finalOutput: []byte(readFixture(t, "completed.json")), want: "type"},
		{name: "missing session identity", stdout: readFixture(t, "missing-session.jsonl"), finalOutput: []byte(readFixture(t, "completed.json")), want: "session"},
		{name: "missing final output", stdout: readFixture(t, "successful.jsonl"), want: "final output"},
		{name: "conflicting session IDs", stdout: readFixture(t, "conflicting-sessions.jsonl"), finalOutput: []byte(readFixture(t, "completed.json")), want: "session"},
		{name: "option-like session ID", stdout: "{\"type\":\"thread.started\",\"thread_id\":\"--last\"}\n", finalOutput: []byte(readFixture(t, "completed.json")), want: "malformed session"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			process := &fakeProcessRunner{stdout: []byte(test.stdout), finalOutput: test.finalOutput}
			runner := mustRunner(t, process)
			if _, err := runner.Run(context.Background(), validRequest(t, agent.AccessWorkspaceWrite)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunnerDoesNotTrustFinalOutputAfterProcessFailure(t *testing.T) {
	process := &fakeProcessRunner{
		finalOutput: []byte(`{"status":"completed","summary":"must not be used"}`),
		err:         errors.New("process failed"),
	}
	runner := mustRunner(t, process)
	if _, err := runner.Run(context.Background(), validRequest(t, agent.AccessWorkspaceWrite)); err == nil || strings.Contains(err.Error(), "must not be used") {
		t.Fatalf("error = %v, want sanitized process failure", err)
	}
}

type fakeProcessRunner struct {
	request     processrun.Request
	stdout      []byte
	finalOutput []byte
	err         error
}

func (runner *fakeProcessRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	runner.request = request
	if request.StdoutObserver != nil && len(runner.stdout) > 0 {
		middle := len(runner.stdout) / 2
		request.StdoutObserver(runner.stdout[:middle])
		request.StdoutObserver(runner.stdout[middle:])
	}
	if len(runner.finalOutput) > 0 {
		outputPath := argumentAfter(request.Argv, "--output-last-message")
		if err := os.WriteFile(outputPath, runner.finalOutput, 0o600); err != nil {
			return processrun.Result{}, err
		}
	}
	return processrun.Result{Stdout: runner.stdout}, runner.err
}

func validRequest(t *testing.T, access agent.AccessLevel) agent.Request {
	t.Helper()
	return agent.Request{
		Worktree:        t.TempDir(),
		Prompt:          "prompt",
		OutputSchema:    filepath.Join(t.TempDir(), "schema.json"),
		OutputDirectory: t.TempDir(),
		Access:          access,
	}
}

func mustRunner(t *testing.T, process processrun.Runner) agent.Runner {
	t.Helper()
	runner, err := codex.NewRunner("codex", process)
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}
	return runner
}

func argumentAfter(arguments []string, flag string) string {
	for index := range arguments {
		if arguments[index] == flag && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join("testdata", name))
}
