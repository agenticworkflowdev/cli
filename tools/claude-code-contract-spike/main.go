// Command claude-code-contract-spike captures sanitized observations from the
// real Claude Code executable. It is a disposable discovery harness that mirrors
// tools/codex-contract-spike, not production adapter code.
//
// Usage:
//
//	go run ./tools/claude-code-contract-spike -output <dir>
//
// The output directory must not already exist. The harness creates throwaway git
// repositories in a temporary directory, runs the real `claude` binary against a
// handful of tiny prompts, and writes sanitized evidence: per-scenario
// events.jsonl, stderr.txt, observation.json, and final-output.json, plus a
// top-level manifest.json. The checked-in README.md alongside the fixtures is
// maintained by hand.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const (
	// promptSentinel is embedded in every prompt and must never survive
	// sanitization into a published fixture.
	promptSentinel = "AWDEV_CLAUDE_SPIKE_PROMPT_SENTINEL_DO_NOT_RECORD"
	// stdoutLimit mirrors the adapter's planned jsonlOutputLimit (32 MiB).
	stdoutLimit = 32 << 20
	stderrLimit = 1 << 20
	// spikeModel keeps runs cheap.
	spikeModel = "claude-haiku-4-5-20251001"
)

// validSchema is the constrained-final-answer schema used by every scenario that
// expects a structured result. It matches the shape the AWDev workflow prompts
// already ask agents to return.
const validSchema = `{"type":"object","additionalProperties":false,"required":["status","summary","question"],` +
	`"properties":{"status":{"type":"string","enum":["completed","blocked"]},` +
	`"summary":{"type":"string"},"question":{"type":"string"}}}`

var (
	credentialPattern     = regexp.MustCompile(`(?i)\b(?:sk-ant-|sk-|sess-|ghp_|github_pat_|xoxb-)[a-z0-9_-]{12,}`)
	oauthAssignmentRegexp = regexp.MustCompile(`(?i)((?:ANTHROPIC|CLAUDE)_[A-Z0-9_]*(?:KEY|TOKEN)\s*[=:]\s*)[^\s"',}]+`)
	socketPathRegexp      = regexp.MustCompile(`/tmp/[A-Za-z0-9._-]*cc-socks[A-Za-z0-9._/-]*`)
	userPathRegexp        = regexp.MustCompile(`(?i)(/Users/|/home/|/root/|[a-z]:\\Users\\)`)
)

type scenario struct {
	name           string
	permissionMode string
	schema         string // literal string passed to --json-schema
	prompt         string
	timeout        time.Duration
	markerName     string
	markerText     string // non-empty => expected to exist with these exact bytes
	// bigFile, when set, is written into the worktree before the run so the
	// model can Read it and produce an oversized tool_result event.
	bigFileName  string
	bigFileBytes int
	expectation  string // human summary recorded in the observation
}

type observation struct {
	Name                    string   `json:"name"`
	Expectation             string   `json:"expectation"`
	Arguments               []string `json:"arguments"`
	WorkingDirectory        string   `json:"working_directory"`
	EnvironmentKeys         []string `json:"environment_keys"`
	PermissionMode          string   `json:"permission_mode"`
	ExitCode                int      `json:"exit_code"`
	Outcome                 string   `json:"outcome"`
	Error                   string   `json:"error,omitempty"`
	SessionID               string   `json:"session_id,omitempty"`
	SessionIDConsistent     *bool    `json:"session_id_consistent,omitempty"`
	EventTypes              []string `json:"event_types"`
	SystemSubtypes          []string `json:"system_subtypes,omitempty"`
	ResultEventPresent      bool     `json:"result_event_present"`
	ResultIsError           *bool    `json:"result_is_error,omitempty"`
	ResultSubtype           string   `json:"result_subtype,omitempty"`
	ResultTerminalReason    string   `json:"result_terminal_reason,omitempty"`
	ResultAPIErrorStatus    string   `json:"result_api_error_status,omitempty"`
	StructuredOutputPresent bool     `json:"structured_output_present"`
	StructuredOutputStatus  string   `json:"structured_output_status,omitempty"`
	PermissionDenials       int      `json:"permission_denials"`
	StdoutBytes             int      `json:"stdout_bytes"`
	StderrBytes             int      `json:"stderr_bytes"`
	LargestJSONLEventBytes  int      `json:"largest_jsonl_event_bytes"`
	FinalOutputPresent      bool     `json:"final_output_present"`
	FinalOutputBytes        int      `json:"final_output_bytes"`
	WorkspaceMarkerExpected *bool    `json:"workspace_marker_expected,omitempty"`
	WorkspaceMarkerPresent  *bool    `json:"workspace_marker_present,omitempty"`
	WorkspaceMarkerMatched  *bool    `json:"workspace_marker_matched,omitempty"`
	ValidationError         string   `json:"validation_error,omitempty"`
}

type manifest struct {
	RecordedAt      string        `json:"recorded_at"`
	ClaudeVersion   string        `json:"claude_version"`
	Platform        string        `json:"platform"`
	Invocation      []string      `json:"invocation"`
	EnvironmentKeys []string      `json:"environment_keys"`
	Scenarios       []observation `json:"scenarios"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var binary string
	var output string
	flag.StringVar(&binary, "claude", "claude", "Claude Code executable to exercise")
	flag.StringVar(&output, "output", "", "new directory for sanitized evidence")
	flag.Parse()
	if strings.TrimSpace(output) == "" {
		return errors.New("-output is required")
	}

	binaryPath, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("locate Claude Code executable: %w", err)
	}
	if binaryPath, err = filepath.Abs(binaryPath); err != nil {
		return fmt.Errorf("resolve Claude Code executable: %w", err)
	}
	if output, err = filepath.Abs(output); err != nil {
		return fmt.Errorf("resolve evidence directory: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("evidence directory already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect evidence directory: %w", err)
	}

	version, err := commandOutput(binaryPath, "--version")
	if err != nil {
		return fmt.Errorf("read Claude Code version: %w", err)
	}

	root, err := os.MkdirTemp("", "awdev-claude-contract-")
	if err != nil {
		return fmt.Errorf("create disposable spike root: %w", err)
	}
	defer os.RemoveAll(root)

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create evidence parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".claude-contract-staging-")
	if err != nil {
		return fmt.Errorf("create evidence staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	environment := authenticationEnvironment()
	envKeys := sortedKeys(environment)

	runner := &processrun.OSRunner{KillGrace: 3 * time.Second, KillWait: 5 * time.Second}
	observations := make([]observation, 0, len(scenarios()))
	for _, current := range scenarios() {
		fmt.Fprintf(os.Stderr, "running %s\n", current.name)
		observed, err := runScenario(runner, root, staging, binaryPath, environment, current)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", current.name, err)
		}
		if observed.ValidationError != "" {
			fmt.Fprintf(os.Stderr, "  validation note: %s\n", observed.ValidationError)
		}
		observations = append(observations, observed)
	}

	record := manifest{
		RecordedAt:      time.Now().UTC().Format(time.RFC3339),
		ClaudeVersion:   strings.TrimSpace(version),
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		Invocation:      invocationTemplate(),
		EnvironmentKeys: envKeys,
		Scenarios:       observations,
	}
	if err := writeJSON(filepath.Join(staging, "manifest.json"), record); err != nil {
		return err
	}

	if err := scanForLeaks(staging, environment); err != nil {
		return fmt.Errorf("SANITIZATION FAILURE: %w", err)
	}

	if err := os.Rename(staging, output); err != nil {
		return fmt.Errorf("publish evidence directory: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote sanitized evidence to %s\n", output)
	return nil
}

func runScenario(runner processrun.Runner, root, evidenceRoot, binary string, environment map[string]string, current scenario) (observation, error) {
	worktree := filepath.Join(root, "workspaces", current.name)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		return observation{}, fmt.Errorf("create worktree: %w", err)
	}
	if err := exec.Command("git", "init", "-q", worktree).Run(); err != nil {
		return observation{}, fmt.Errorf("initialize worktree: %w", err)
	}
	if current.bigFileName != "" {
		if err := os.WriteFile(filepath.Join(worktree, current.bigFileName), fillerFile(current.bigFileBytes), 0o644); err != nil {
			return observation{}, fmt.Errorf("write filler file: %w", err)
		}
	}

	sessionID, err := newUUID()
	if err != nil {
		return observation{}, fmt.Errorf("generate session id: %w", err)
	}

	arguments := []string{
		binary,
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", current.schema,
		"--permission-mode", current.permissionMode,
		"--permission-prompts", "none",
		"--model", spikeModel,
		"--session-id", sessionID,
		"--add-dir", worktree,
	}

	timeout := current.timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	result, runErr := runner.Run(ctx, processrun.Request{
		Directory:        worktree,
		Argv:             arguments,
		Stdin:            []byte(current.prompt),
		Environment:      environment,
		CleanEnvironment: true,
		StdoutLimit:      stdoutLimit,
		StderrLimit:      stderrLimit,
	})
	cancel()
	if result.StdoutTruncated || result.StderrTruncated {
		return observation{}, errors.New("captured output exceeded the harness limit")
	}

	redact := newRedactor(root, binary, sessionID, environment)
	sanitizedStdout := redact(result.Stdout)
	sanitizedStderr := redact(result.Stderr)

	stream := inspectStream(sanitizedStdout)
	finalOutput := extractStructuredOutput(stream.resultEventRaw)

	scenarioEvidence := filepath.Join(evidenceRoot, current.name)
	if err := os.MkdirAll(scenarioEvidence, 0o755); err != nil {
		return observation{}, fmt.Errorf("create scenario evidence directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(scenarioEvidence, "events.jsonl"), sanitizedStdout, 0o644); err != nil {
		return observation{}, fmt.Errorf("write events evidence: %w", err)
	}
	if err := os.WriteFile(filepath.Join(scenarioEvidence, "stderr.txt"), sanitizedStderr, 0o644); err != nil {
		return observation{}, fmt.Errorf("write stderr evidence: %w", err)
	}
	if len(finalOutput) > 0 {
		if err := os.WriteFile(filepath.Join(scenarioEvidence, "final-output.json"), finalOutput, 0o644); err != nil {
			return observation{}, fmt.Errorf("write final-output evidence: %w", err)
		}
	}

	outcome := "completed"
	if runErr != nil {
		outcome = "failed"
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			outcome = "canceled"
		}
	}

	observed := observation{
		Name:                    current.name,
		Expectation:             current.expectation,
		Arguments:               sanitizedArguments(arguments, binary, current.schema, worktree),
		WorkingDirectory:        "$WORKSPACE",
		EnvironmentKeys:         sortedKeys(environment),
		PermissionMode:          current.permissionMode,
		ExitCode:                result.ExitCode,
		Outcome:                 outcome,
		SessionID:               stream.sessionID,
		EventTypes:              stream.eventTypes,
		SystemSubtypes:          stream.systemSubtypes,
		ResultEventPresent:      stream.resultPresent,
		ResultSubtype:           stream.resultSubtype,
		ResultTerminalReason:    stream.resultTerminalReason,
		ResultAPIErrorStatus:    stream.resultAPIErrorStatus,
		StructuredOutputPresent: len(finalOutput) > 0,
		StructuredOutputStatus:  stream.structuredStatus,
		PermissionDenials:       stream.permissionDenials,
		StdoutBytes:             len(sanitizedStdout),
		StderrBytes:             len(sanitizedStderr),
		LargestJSONLEventBytes:  stream.largestEvent,
		FinalOutputPresent:      len(finalOutput) > 0,
		FinalOutputBytes:        len(finalOutput),
	}
	if stream.sessionID != "" {
		consistent := stream.sessionIDConsistent
		observed.SessionIDConsistent = &consistent
	}
	if stream.resultPresent {
		isError := stream.resultIsError
		observed.ResultIsError = &isError
	}
	if runErr != nil {
		observed.Error = strings.TrimSpace(string(redact([]byte(runErr.Error()))))
	}

	if current.markerName != "" {
		marker, markerErr := os.ReadFile(filepath.Join(worktree, current.markerName))
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return observation{}, fmt.Errorf("inspect workspace marker: %w", markerErr)
		}
		present := markerErr == nil
		expected := current.markerText != ""
		matched := present && string(marker) == current.markerText
		observed.WorkspaceMarkerExpected = &expected
		observed.WorkspaceMarkerPresent = &present
		observed.WorkspaceMarkerMatched = &matched
	}

	observed.ValidationError = validateScenario(current, observed, runErr)

	if err := writeJSON(filepath.Join(scenarioEvidence, "observation.json"), observed); err != nil {
		return observation{}, err
	}
	return observed, nil
}

func scenarios() []scenario {
	// withSentinel appends a benign trace tag. It is the canary the fixture
	// scanner (here and in the adapter tests) looks for to prove the harness's
	// own injected text never lands in a published fixture. It is written to
	// read like an inert trace id so the model ignores it rather than treating
	// it as an injected instruction.
	withSentinel := func(instruction string) string {
		return instruction + "\n\n[trace:" + promptSentinel + "]\n"
	}
	return []scenario{
		{
			name: "successful-structured",
			// acceptEdits, not plan: plan mode injects a planning-workflow system
			// prompt that fights a bare "return this structured object" request
			// (see the read-only scenario and the README for plan-mode behaviour).
			permissionMode: "acceptEdits",
			schema:         validSchema,
			prompt: withSentinel(`Is the number seven a prime number? Answer with structured output: ` +
				`set status to "completed", summary to a one-sentence answer, and question to "".`),
			expectation: "exit 0, terminal result event carrying structured_output, is_error false",
		},
		{
			name:           "read-only",
			permissionMode: "plan",
			schema:         validSchema,
			markerName:     "read-only-marker.txt",
			prompt: withSentinel(`Use the Bash tool to run exactly this command, then report what happened: ` +
				`printf overwrite > read-only-marker.txt` + "\n" +
				`Then return status "completed", a short summary, and an empty question.`),
			expectation: "plan mode auto-denies the write; independent check finds no marker; run still exits 0",
		},
		{
			name:           "workspace-write",
			permissionMode: "acceptEdits",
			schema:         validSchema,
			markerName:     "workspace-write-marker.txt",
			markerText:     "workspace-write-ok",
			prompt: withSentinel(`Use the Bash tool to run exactly this command: ` +
				`printf '%s' workspace-write-ok > workspace-write-marker.txt` + "\n" +
				`Verify the file contents, then return status "completed", a short summary, and an empty question.`),
			expectation: "acceptEdits allows the file-redirect write; independent check finds the exact marker bytes",
		},
		{
			name:           "schema-rejected",
			permissionMode: "plan",
			schema:         `{"type":"object", NOT_VALID_JSON}`,
			prompt:         withSentinel(`Return status "completed" and a one word summary.`),
			expectation:    "CLI rejects the malformed --json-schema up front: exit 1, stderr error, empty stdout, no API call",
		},
		{
			name:           "blocked",
			permissionMode: "plan",
			schema:         validSchema,
			prompt: withSentinel(`A code change must target a specific release version, but the request did ` +
				`not name one, so you cannot proceed. Answer with structured output: set status to "blocked", ` +
				`summary to "the target release version was not provided", and question to ` +
				`"Which release version should this target?".`),
			expectation: "a blocked result is an ordinary structured_output payload; exit 0, is_error false",
		},
		{
			name:           "cancellation",
			permissionMode: "plan",
			schema:         validSchema,
			timeout:        5 * time.Second,
			prompt: withSentinel(`Use the Bash tool to run exactly this command and wait for it to finish: ` +
				`sleep 30` + "\n" + `After it finishes, return status "completed" with a short summary and an empty question.`),
			expectation: "SIGTERM after ~5s truncates the stream with no terminal result event; classified from ctx error",
		},
		{
			name:           "large-event",
			permissionMode: "plan",
			schema:         validSchema,
			bigFileName:    "big.txt",
			bigFileBytes:   200_000,
			prompt: withSentinel(`Use the Read tool to read big.txt in full. ` +
				`Then return status "completed", summary "inspected the large file", and an empty question.`),
			expectation: "the Read tool_result is a single NDJSON event larger than bufio.Scanner's 64 KiB default",
		},
	}
}

func validateScenario(current scenario, observed observation, runErr error) string {
	switch current.name {
	case "successful-structured", "blocked":
		if runErr != nil {
			return fmt.Sprintf("expected a clean run, got %v", runErr)
		}
		if observed.ExitCode != 0 || !observed.ResultEventPresent || !observed.StructuredOutputPresent {
			return "expected exit 0 with a terminal result event and structured_output"
		}
		if observed.ResultIsError != nil && *observed.ResultIsError {
			return "result event reported is_error true"
		}
		if current.name == "blocked" && observed.StructuredOutputStatus != "blocked" {
			return fmt.Sprintf("expected structured_output.status blocked, got %q", observed.StructuredOutputStatus)
		}
	case "read-only":
		if observed.WorkspaceMarkerPresent == nil || *observed.WorkspaceMarkerPresent {
			return "plan mode was expected to prevent the marker write"
		}
	case "workspace-write":
		if observed.WorkspaceMarkerMatched == nil || !*observed.WorkspaceMarkerMatched {
			return "acceptEdits was expected to create the marker with exact bytes"
		}
	case "schema-rejected":
		if runErr == nil || observed.ExitCode == 0 || observed.ResultEventPresent || observed.StructuredOutputPresent {
			return "expected a non-zero exit with no events and no structured output"
		}
	case "cancellation":
		if !errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, context.Canceled) {
			return fmt.Sprintf("expected a context error, got %v", runErr)
		}
		if observed.ResultEventPresent || observed.StructuredOutputPresent {
			return "a canceled run should not carry a terminal result event"
		}
	case "large-event":
		if observed.LargestJSONLEventBytes <= 64*1024 {
			return fmt.Sprintf("largest event was %d bytes, expected more than 64 KiB", observed.LargestJSONLEventBytes)
		}
	}
	return ""
}

// streamFacts is the distilled view of one sanitized stdout stream.
type streamFacts struct {
	eventTypes           []string
	systemSubtypes       []string
	sessionID            string
	sessionIDConsistent  bool
	largestEvent         int
	resultPresent        bool
	resultEventRaw       []byte
	resultIsError        bool
	resultSubtype        string
	resultTerminalReason string
	resultAPIErrorStatus string
	structuredStatus     string
	permissionDenials    int
}

func inspectStream(contents []byte) streamFacts {
	facts := streamFacts{sessionIDConsistent: true}
	seenType := map[string]bool{}
	seenSubtype := map[string]bool{}
	for _, line := range bytes.Split(contents, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > facts.largestEvent {
			facts.largestEvent = len(line)
		}
		var event struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			IsError   bool            `json:"is_error"`
			Terminal  string          `json:"terminal_reason"`
			APIError  json.RawMessage `json:"api_error_status"`
			Denials   []struct{}      `json:"permission_denials"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.Type != "" && !seenType[event.Type] {
			seenType[event.Type] = true
			facts.eventTypes = append(facts.eventTypes, event.Type)
		}
		if event.Type == "system" && event.Subtype != "" && !seenSubtype[event.Subtype] {
			seenSubtype[event.Subtype] = true
			facts.systemSubtypes = append(facts.systemSubtypes, event.Subtype)
		}
		if event.SessionID != "" {
			if facts.sessionID == "" {
				facts.sessionID = event.SessionID
			} else if facts.sessionID != event.SessionID {
				facts.sessionIDConsistent = false
			}
		}
		if event.Type == "result" {
			facts.resultPresent = true
			facts.resultEventRaw = append([]byte(nil), line...)
			facts.resultIsError = event.IsError
			facts.resultSubtype = event.Subtype
			facts.resultTerminalReason = event.Terminal
			if len(event.APIError) > 0 && !bytes.Equal(bytes.TrimSpace(event.APIError), []byte("null")) {
				facts.resultAPIErrorStatus = string(bytes.Trim(event.APIError, `"`))
			}
			facts.permissionDenials = len(event.Denials)
		}
	}
	if facts.resultEventRaw != nil {
		var structured struct {
			StructuredOutput struct {
				Status string `json:"status"`
			} `json:"structured_output"`
		}
		if err := json.Unmarshal(facts.resultEventRaw, &structured); err == nil {
			facts.structuredStatus = structured.StructuredOutput.Status
		}
	}
	return facts
}

// extractStructuredOutput pretty-prints the structured_output object from a
// terminal result event, or returns nil when the model produced none.
func extractStructuredOutput(resultEvent []byte) []byte {
	if len(resultEvent) == 0 {
		return nil
	}
	var envelope struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(resultEvent, &envelope); err != nil {
		return nil
	}
	trimmed := bytes.TrimSpace(envelope.StructuredOutput)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, trimmed, "", "  "); err != nil {
		return nil
	}
	pretty.WriteByte('\n')
	return pretty.Bytes()
}

// authenticationEnvironment builds the clean allowlist the adapter will use.
// Only key names are ever recorded; values stay out of every fixture.
func authenticationEnvironment() map[string]string {
	environment := map[string]string{"AWDEV_WORKER": "1"}
	for _, name := range []string{
		"HOME",
		"PATH",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_API_KEY",
	} {
		if value := os.Getenv(name); value != "" {
			environment[name] = value
		}
	}
	return environment
}

func invocationTemplate() []string {
	return []string{
		"$CLAUDE",
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", "$SCHEMA",
		"--permission-mode", "<plan|acceptEdits>",
		"--permission-prompts", "none",
		"--model", spikeModel,
		"--session-id", "$SESSION",
		"--add-dir", "$WORKSPACE",
	}
}

func newRedactor(root, binary, sessionID string, environment map[string]string) func([]byte) []byte {
	slug := func(path string) string { return strings.ReplaceAll(path, "/", "-") }
	replacements := [][2]string{}
	if configDir := environment["CLAUDE_CONFIG_DIR"]; configDir != "" {
		replacements = append(replacements, [2]string{configDir, "$CONFIG_DIR"}, [2]string{slug(configDir), "$CONFIG_DIR"})
	}
	replacements = append(replacements,
		[2]string{root, "$WORKSPACE"},
		[2]string{slug(root), "$WORKSPACE"},
		[2]string{binary, "$CLAUDE"},
		[2]string{sessionID, "$SESSION"},
		[2]string{promptSentinel, "[PROMPT_REDACTED]"},
	)
	if home := environment["HOME"]; home != "" {
		replacements = append(replacements, [2]string{home, "$HOME"}, [2]string{slug(home), "$HOME"})
	}
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if value := environment[name]; value != "" {
			replacements = append(replacements, [2]string{value, "[CREDENTIAL_REDACTED]"})
		}
	}
	return func(contents []byte) []byte {
		clean := string(contents)
		for _, replacement := range replacements {
			if replacement[0] == "" {
				continue
			}
			clean = strings.ReplaceAll(clean, replacement[0], replacement[1])
		}
		clean = socketPathRegexp.ReplaceAllLiteralString(clean, "$RUNTIME")
		clean = credentialPattern.ReplaceAllString(clean, "[CREDENTIAL_REDACTED]")
		clean = oauthAssignmentRegexp.ReplaceAllString(clean, "${1}[REDACTED]")
		return []byte(clean)
	}
}

func sanitizedArguments(arguments []string, binary, schema, worktree string) []string {
	replacements := map[string]string{
		binary:   "$CLAUDE",
		schema:   "$SCHEMA",
		worktree: "$WORKSPACE",
	}
	clean := make([]string, len(arguments))
	for index, argument := range arguments {
		clean[index] = argument
		if replacement, ok := replacements[argument]; ok {
			clean[index] = replacement
		}
	}
	return clean
}

// scanForLeaks re-reads every published fixture and fails loudly on
// credential-shaped strings, absolute user paths, the prompt sentinel, or the
// live OAuth/API token value.
func scanForLeaks(directory string, environment map[string]string) error {
	secrets := []string{}
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if value := strings.TrimSpace(environment[name]); value != "" {
			secrets = append(secrets, value)
		}
	}
	var findings []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		label := strings.TrimPrefix(path, directory+string(os.PathSeparator))
		if loc := userPathRegexp.FindIndex(contents); loc != nil {
			findings = append(findings, fmt.Sprintf("%s: absolute user path near %q", label, snippet(contents, loc[0])))
		}
		if loc := credentialPattern.FindIndex(contents); loc != nil {
			findings = append(findings, fmt.Sprintf("%s: credential-shaped string near %q", label, snippet(contents, loc[0])))
		}
		if bytes.Contains(contents, []byte("AWDEV_CLAUDE_SPIKE_PROMPT_SENTINEL")) {
			findings = append(findings, label+": prompt sentinel survived sanitization")
		}
		for _, secret := range secrets {
			if bytes.Contains(contents, []byte(secret)) {
				findings = append(findings, label+": live credential value present")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return errors.New("\n  " + strings.Join(findings, "\n  "))
	}
	return nil
}

func snippet(contents []byte, at int) string {
	start := at - 16
	if start < 0 {
		start = 0
	}
	end := at + 24
	if end > len(contents) {
		end = len(contents)
	}
	return string(contents[start:end])
}

func fillerFile(size int) []byte {
	const line = "the quick brown fox jumps over the lazy dog while the sleepy cat ignores everything around it " +
		"0123456789 0123456789 0123456789 0123456789 0123456789 0123456789 0123456789 0123456789\n"
	var builder strings.Builder
	for builder.Len() < size {
		builder.WriteString(line)
	}
	return []byte(builder.String())
}

func newUUID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16]), nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func commandOutput(name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
