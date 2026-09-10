package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/agent/claudecode"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRunnerInvokesClaudeCodeThroughTheVerifiedContract(t *testing.T) {
	worktree := t.TempDir()
	outputDirectory := t.TempDir()
	schemaPath := filepath.Join(t.TempDir(), "agent-result.schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(binary, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	configDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-oauth-token")
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("AWDEV_UNRELATED_SECRET", "must-not-leak")

	runner, err := claudecode.NewRunner(binary, "claude-sonnet-test", processrun.NewRunner())
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
		t.Fatalf("run claude code: %v", err)
	}

	argv := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(worktree, "argv.txt"))), "\n")
	want := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", `{"type":"object"}`,
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
		"--model", "claude-sonnet-test",
		"--session-id", argvValue(argv, "--session-id"),
		"--add-dir", worktree,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v\nwant %#v", argv, want)
	}
	sessionID := argvValue(argv, "--session-id")
	if !uuidV4Pattern.MatchString(sessionID) {
		t.Fatalf("session id %q is not a UUIDv4", sessionID)
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
	for _, want := range []string{
		"AWDEV_WORKER=1",
		"CLAUDE_CODE_OAUTH_TOKEN=test-oauth-token",
		"ANTHROPIC_API_KEY=test-anthropic-key",
		"ANTHROPIC_BASE_URL=https://anthropic.example",
		"CLAUDE_CONFIG_DIR=" + configDir,
		"HOME=" + home,
		"PATH=",
	} {
		if !strings.Contains(environment, want) {
			t.Errorf("environment does not contain %q:\n%s", want, environment)
		}
	}
	for _, forbidden := range []string{"AWDEV_UNRELATED_SECRET", "CLAUDE_CODE_ENTRYPOINT"} {
		if strings.Contains(environment, forbidden) {
			t.Fatalf("environment leaked %q to Claude Code:\n%s", forbidden, environment)
		}
	}

	if result.SessionID != sessionID {
		t.Fatalf("run result session id = %q, want %q", result.SessionID, sessionID)
	}
	if got := string(result.FinalOutput); got != `{"status":"completed","summary":"specification written","question":""}` {
		t.Fatalf("final output = %q", got)
	}
	if len(result.Progress) == 0 {
		t.Fatalf("run result has no progress: %#v", result)
	}
}

func TestRunnerResumesAnExistingClaudeCodeSession(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	stream := `{"type":"system","subtype":"init","session_id":"` + sessionID + `"}` + "\n" +
		`{"type":"result","session_id":"` + sessionID + `","is_error":false,"structured_output":{"status":"completed","summary":"done"}}` + "\n"
	process := &fakeProcessRunner{stdout: []byte(stream)}
	runner := mustRunner(t, process)
	request := validRequest(t, agent.AccessWorkspaceWrite)
	request.ResumeSessionID = sessionID

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := argvValue(process.request.Argv, "--resume"); got != sessionID {
		t.Fatalf("--resume = %q, want %q; argv=%#v", got, sessionID, process.request.Argv)
	}
	if got := argvValue(process.request.Argv, "--session-id"); got != "" {
		t.Fatalf("resumed run created a new session %q; argv=%#v", got, process.request.Argv)
	}
	if result.SessionID != sessionID {
		t.Fatalf("session id = %q, want %q", result.SessionID, sessionID)
	}
}

func TestRunnerSelectsPlanModeForReadOnlyAccessAndOmitsTheModelFlag(t *testing.T) {
	worktree := t.TempDir()
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(binary, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())

	runner, err := claudecode.NewRunner(binary, "", processrun.NewRunner())
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}
	if _, err := runner.Run(context.Background(), agent.Request{
		Worktree:        worktree,
		Prompt:          "prompt",
		OutputSchema:    schemaPath,
		OutputDirectory: t.TempDir(),
		Access:          agent.AccessReadOnly,
	}); err != nil {
		t.Fatalf("run claude code: %v", err)
	}

	argv := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(worktree, "argv.txt"))), "\n")
	want := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", `{"type":"object"}`,
		"--permission-mode", "plan",
		"--permission-prompts", "none",
		"--session-id", argvValue(argv, "--session-id"),
		"--add-dir", worktree,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v\nwant %#v", argv, want)
	}
	if argvValue(argv, "--model") != "" {
		t.Fatalf("read-only run passed --model: %#v", argv)
	}
}

func TestRunnerStripsMetaSchemaKeysClaudeCodeCannotResolve(t *testing.T) {
	worktree := t.TempDir()
	schemaPath := filepath.Join(t.TempDir(), "agent-result.schema.json")
	repositorySchema := `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://agenticworkflow.dev/schemas/agent-result.schema.json",
  "title": "Agent result",
  "type": "object",
  "additionalProperties": false,
  "required": ["status"],
  "properties": {"status": {"type": "string"}}
}`
	if err := os.WriteFile(schemaPath, []byte(repositorySchema), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(binary, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())

	runner, err := claudecode.NewRunner(binary, "", processrun.NewRunner())
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}
	if _, err := runner.Run(context.Background(), agent.Request{
		Worktree:        worktree,
		Prompt:          "prompt",
		OutputSchema:    schemaPath,
		OutputDirectory: t.TempDir(),
		Access:          agent.AccessReadOnly,
	}); err != nil {
		t.Fatalf("run claude code: %v", err)
	}

	argv := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(worktree, "argv.txt"))), "\n")
	passed := argvValue(argv, "--json-schema")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(passed), &decoded); err != nil {
		t.Fatalf("passed --json-schema is not JSON: %v (%q)", err, passed)
	}
	if _, ok := decoded["$schema"]; ok {
		t.Errorf("--json-schema still carries $schema: %q", passed)
	}
	if _, ok := decoded["$id"]; ok {
		t.Errorf("--json-schema still carries $id: %q", passed)
	}
	for _, keep := range []string{"type", "required", "properties", "additionalProperties", "title"} {
		if _, ok := decoded[keep]; !ok {
			t.Errorf("--json-schema dropped %q: %q", keep, passed)
		}
	}
}

func TestRunnerStreamsReadableProgressEventsWhileTheProcessRuns(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret-value")
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}`,
		`{"type":"assistant","session_id":"11111111-1111-4111-8111-111111111111","message":{"content":[{"type":"thinking","thinking":"Inspecting the project"}]}}`,
		`{"type":"assistant","session_id":"11111111-1111-4111-8111-111111111111","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","session_id":"11111111-1111-4111-8111-111111111111","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok secret-value\nExit code 0"}]},"tool_use_result":{"stdout":"ok secret-value\n","stderr":""}}`,
		`{"type":"assistant","session_id":"11111111-1111-4111-8111-111111111111","message":{"content":[{"type":"text","text":"Implementation complete"}]}}`,
		`{"type":"result","session_id":"11111111-1111-4111-8111-111111111111","is_error":false,"structured_output":{"status":"completed","summary":"done"},"result":"{}"}`,
	}, "\n") + "\n"

	process := &fakeProcessRunner{stdout: []byte(stream)}
	runner := mustRunner(t, process)
	var streamed []agent.ProgressEvent
	request := validRequest(t, agent.AccessWorkspaceWrite)
	request.Progress = func(event agent.ProgressEvent) { streamed = append(streamed, event) }

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	zero := 0
	want := []agent.ProgressEvent{
		{Type: "assistant", Kind: agent.ProgressReasoning, Message: "Inspecting the project"},
		{Type: "assistant", Kind: agent.ProgressCommand, Message: "go test ./..."},
		{Type: "user", Kind: agent.ProgressCommandOutput, Message: "ok [REDACTED]\n", ExitCode: &zero},
		{Type: "assistant", Kind: agent.ProgressMessage, Message: "Implementation complete"},
	}
	if !reflect.DeepEqual(streamed, want) {
		t.Fatalf("streamed progress = %#v\nwant %#v", streamed, want)
	}
	if string(result.FinalOutput) != `{"status":"completed","summary":"done"}` {
		t.Fatalf("final output = %q", string(result.FinalOutput))
	}
}

func TestRunnerRejectsRunsThatDoNotProduceAContractResult(t *testing.T) {
	session := `"session_id":"22222222-2222-4222-8222-222222222222"`
	tests := []struct {
		name   string
		stdout string
		stderr string
		err    error
		want   string
	}{
		{
			name:   "prose instead of structured output",
			stdout: `{"type":"system","subtype":"init",` + session + `}` + "\n" + `{"type":"result",` + session + `,"is_error":false,"result":"here is some prose"}` + "\n",
			want:   "structured result",
		},
		{
			name:   "reported api error",
			stdout: `{"type":"system","subtype":"init",` + session + `}` + "\n" + `{"type":"result",` + session + `,"is_error":true,"api_error_status":404,"result":"There is an issue with the selected model"}` + "\n",
			want:   "api_error_status 404",
		},
		{
			name:   "stream ends without a result event",
			stdout: `{"type":"system","subtype":"init",` + session + `}` + "\n" + `{"type":"assistant",` + session + `,"message":{"content":[{"type":"text","text":"working"}]}}` + "\n",
			want:   "without a result event",
		},
		{
			name:   "conflicting session identities",
			stdout: `{"type":"system","subtype":"init",` + session + `}` + "\n" + `{"type":"result","session_id":"33333333-3333-4333-8333-333333333333","is_error":false,"structured_output":{"status":"completed","summary":"done"}}` + "\n",
			want:   "conflicting session",
		},
		{
			name:   "missing session identity",
			stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}` + "\n",
			want:   "session identity is missing",
		},
		{
			name:   "option-like session identity",
			stdout: `{"type":"system","subtype":"init","session_id":"--resume"}` + "\n" + `{"type":"result","session_id":"--resume","is_error":false,"structured_output":{"status":"completed","summary":"done"}}` + "\n",
			want:   "malformed session identity",
		},
		{
			name:   "malformed stream event",
			stdout: `{"type":"system","subtype":"init",` + session + `}` + "\n" + `{not json}` + "\n",
			want:   "decode Claude Code stream",
		},
		{
			name:   "missing event type",
			stdout: `{` + session + `}` + "\n",
			want:   "event type is missing",
		},
		{
			name:   "cli rejected the schema",
			stderr: "Error: --json-schema is not valid JSON: unexpected token\n",
			err:    &processrun.ExitError{Argv: []string{"claude"}, Result: processrun.Result{ExitCode: 1}},
			want:   "not valid JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			process := &fakeProcessRunner{stdout: []byte(test.stdout), stderr: []byte(test.stderr), err: test.err}
			runner := mustRunner(t, process)
			_, err := runner.Run(context.Background(), validRequest(t, agent.AccessWorkspaceWrite))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRunnerPrefersTheContextErrorWhenTheStreamIsCutShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process := &fakeProcessRunner{
		stdout: []byte(`{"type":"system","subtype":"init","session_id":"44444444-4444-4444-8444-444444444444"}` + "\n"),
		err:    context.Canceled,
	}
	runner := mustRunner(t, process)
	_, err := runner.Run(ctx, validRequest(t, agent.AccessReadOnly))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "run Claude Code") {
		t.Fatalf("error = %v, want a run Claude Code prefix", err)
	}
}

func TestRunnerParsesLargeAndUnknownStreamEventsWithoutAScannerLimit(t *testing.T) {
	session := `"session_id":"55555555-5555-4555-8555-555555555555"`
	large := strings.Repeat("x", 200*1024)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init",` + session + `}`,
		`{"type":"rate_limit_event",` + session + `}`,
		`{"type":"future_event","subtype":"experimental",` + session + `,"payload":"` + large + `"}`,
		`{"type":"assistant",` + session + `,"message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
		`{"type":"user",` + session + `,"message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + large + `"}]},"tool_use_result":{"type":"text"}}`,
		`{"type":"result",` + session + `,"is_error":false,"structured_output":{"status":"completed","summary":"done"}}`,
	}, "\n") + "\n"

	process := &fakeProcessRunner{stdout: []byte(stream)}
	runner := mustRunner(t, process)
	result, err := runner.Run(context.Background(), validRequest(t, agent.AccessReadOnly))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.SessionID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("session id = %q", result.SessionID)
	}
	if string(result.FinalOutput) != `{"status":"completed","summary":"done"}` {
		t.Fatalf("final output = %q", string(result.FinalOutput))
	}
	if process.request.Argv[argvIndex(process.request.Argv, "--permission-mode")+1] != "plan" {
		t.Fatalf("permission mode = %#v", process.request.Argv)
	}
}

func TestNewRunnerValidatesItsInputs(t *testing.T) {
	if _, err := claudecode.NewRunner("", "model", processrun.NewRunner()); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("error = %v, want a binary complaint", err)
	}
	if _, err := claudecode.NewRunner("claude", "model", nil); err == nil || !strings.Contains(err.Error(), "process runner") {
		t.Fatalf("error = %v, want a process runner complaint", err)
	}
}

func TestRunnerRejectsInvalidRequests(t *testing.T) {
	runner := mustRunner(t, &fakeProcessRunner{})
	base := validRequest(t, agent.AccessWorkspaceWrite)

	relative := base
	relative.Worktree = "relative/path"
	if _, err := runner.Run(context.Background(), relative); err == nil || !strings.Contains(err.Error(), "absolute clean path") {
		t.Fatalf("error = %v, want an absolute clean path complaint", err)
	}

	emptyPrompt := base
	emptyPrompt.Prompt = ""
	if _, err := runner.Run(context.Background(), emptyPrompt); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("error = %v, want a prompt complaint", err)
	}

	badAccess := base
	badAccess.Access = "full"
	if _, err := runner.Run(context.Background(), badAccess); err == nil || !strings.Contains(err.Error(), "access level") {
		t.Fatalf("error = %v, want an access level complaint", err)
	}

	missingSchema := base
	missingSchema.OutputSchema = filepath.Join(t.TempDir(), "absent.json")
	if _, err := runner.Run(context.Background(), missingSchema); err == nil || !strings.Contains(err.Error(), "output schema") {
		t.Fatalf("error = %v, want an output schema complaint", err)
	}
}

const fakeClaudeScript = `#!/bin/sh
printf '%s\n' "$@" > "$PWD/argv.txt"
printf '%s' "$PWD" > "$PWD/cwd.txt"
/bin/cat > "$PWD/stdin.txt"
/usr/bin/env | /usr/bin/sort > "$PWD/environment.txt"
session=
previous=
for argument in "$@"; do
  if [ "$previous" = "--session-id" ]; then session="$argument"; fi
  previous="$argument"
done
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$session"
printf '{"type":"assistant","session_id":"%s","message":{"content":[{"type":"text","text":"writing specification"}]}}\n' "$session"
printf '{"type":"result","session_id":"%s","is_error":false,"structured_output":{"status":"completed","summary":"specification written","question":""},"result":"{}"}\n' "$session"
`

type fakeProcessRunner struct {
	request processrun.Request
	stdout  []byte
	stderr  []byte
	err     error
}

func (runner *fakeProcessRunner) Run(_ context.Context, request processrun.Request) (processrun.Result, error) {
	runner.request = request
	if request.StdoutObserver != nil && len(runner.stdout) > 0 {
		middle := len(runner.stdout) / 2
		request.StdoutObserver(runner.stdout[:middle])
		request.StdoutObserver(runner.stdout[middle:])
	}
	return processrun.Result{Stdout: runner.stdout, Stderr: runner.stderr}, runner.err
}

func validRequest(t *testing.T, access agent.AccessLevel) agent.Request {
	t.Helper()
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return agent.Request{
		Worktree:        t.TempDir(),
		Prompt:          "prompt",
		OutputSchema:    schemaPath,
		OutputDirectory: t.TempDir(),
		Access:          access,
	}
}

func mustRunner(t *testing.T, process processrun.Runner) agent.Runner {
	t.Helper()
	runner, err := claudecode.NewRunner("claude", "", process)
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}
	return runner
}

func argvIndex(arguments []string, flag string) int {
	for index := range arguments {
		if arguments[index] == flag {
			return index
		}
	}
	return -1
}

func argvValue(arguments []string, flag string) string {
	if index := argvIndex(arguments, flag); index >= 0 && index+1 < len(arguments) {
		return arguments[index+1]
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
