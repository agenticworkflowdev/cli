// Command codex-contract-spike captures sanitized observations from the real
// Codex executable. It is a disposable discovery harness, not production
// adapter code.
package main

import (
	"bytes"
	"context"
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
	promptSecret = "AWDEV_SPIKE_PROMPT_SECRET_DO_NOT_RECORD"
	outputLimit  = 32 << 20
)

var (
	credentialPattern = regexp.MustCompile(`(?i)\b(?:sk-|sess-|ghp_|github_pat_|xoxb-)[a-z0-9_-]{8,}`)
	assignmentPattern = regexp.MustCompile(`(?i)((?:OPENAI|CODEX)_[A-Z0-9_]*(?:KEY|TOKEN)\s*[=:]\s*)[^\s"']+`)
)

type scenario struct {
	name       string
	sandbox    string
	prompt     string
	schema     string
	timeout    time.Duration
	markerName string
	markerText string
	validate   func(observation, error, []byte) error
}

type observation struct {
	Name                    string   `json:"name"`
	Arguments               []string `json:"arguments"`
	WorkingDirectory        string   `json:"working_directory"`
	EnvironmentKeys         []string `json:"environment_keys"`
	Sandbox                 string   `json:"sandbox"`
	ExitCode                int      `json:"exit_code"`
	Outcome                 string   `json:"outcome"`
	Error                   string   `json:"error,omitempty"`
	SessionID               string   `json:"session_id,omitempty"`
	EventTypes              []string `json:"event_types"`
	StdoutBytes             int      `json:"stdout_bytes"`
	StderrBytes             int      `json:"stderr_bytes"`
	LargestJSONLEventBytes  int      `json:"largest_jsonl_event_bytes"`
	FinalOutputPresent      bool     `json:"final_output_present"`
	FinalOutputBytes        int      `json:"final_output_bytes"`
	WorkspaceMarkerExpected *bool    `json:"workspace_marker_expected,omitempty"`
	WorkspaceMarkerPresent  *bool    `json:"workspace_marker_present,omitempty"`
	WorkspaceMarkerMatched  *bool    `json:"workspace_marker_matched,omitempty"`
}

type manifest struct {
	RecordedAt   string        `json:"recorded_at"`
	CodexVersion string        `json:"codex_version"`
	Platform     string        `json:"platform"`
	Scenarios    []observation `json:"scenarios"`
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
	flag.StringVar(&binary, "codex", "codex", "Codex executable to exercise")
	flag.StringVar(&output, "output", "", "new directory for sanitized evidence")
	flag.Parse()
	if strings.TrimSpace(output) == "" {
		return errors.New("-output is required")
	}

	binaryPath, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("locate Codex executable: %w", err)
	}
	binaryPath, err = filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("resolve Codex executable: %w", err)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve evidence directory: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("evidence directory already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect evidence directory: %w", err)
	}

	version, err := commandOutput(binaryPath, "--version")
	if err != nil {
		return fmt.Errorf("read Codex version: %w", err)
	}
	root, err := os.MkdirTemp("", "awdev-codex-contract-")
	if err != nil {
		return fmt.Errorf("create disposable spike root: %w", err)
	}
	defer os.RemoveAll(root)

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create evidence parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".codex-contract-staging-")
	if err != nil {
		return fmt.Errorf("create evidence staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	environment := contractEnvironment()
	redactor := newRedactor(root, binaryPath)
	observations := make([]observation, 0, len(scenarios()))
	for _, current := range scenarios() {
		fmt.Fprintf(os.Stderr, "running %s\n", current.name)
		observed, err := runScenario(root, staging, binaryPath, environment, redactor, current)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", current.name, err)
		}
		observations = append(observations, observed)
	}

	record := manifest{
		RecordedAt:   time.Now().UTC().Format(time.RFC3339),
		CodexVersion: strings.TrimSpace(version),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Scenarios:    observations,
	}
	if err := writeJSON(filepath.Join(staging, "manifest.json"), record); err != nil {
		return err
	}
	if err := os.Rename(staging, output); err != nil {
		return fmt.Errorf("publish evidence directory: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote sanitized evidence to %s\n", output)
	return nil
}

func runScenario(root, evidenceRoot, binary string, environment map[string]string, redact func([]byte) []byte, current scenario) (observation, error) {
	workspace := filepath.Join(root, "workspaces", current.name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return observation{}, fmt.Errorf("create workspace: %w", err)
	}
	if err := exec.Command("git", "init", "-q", workspace).Run(); err != nil {
		return observation{}, fmt.Errorf("initialize workspace: %w", err)
	}
	schemaPath := filepath.Join(root, "schemas", current.name+".json")
	if err := os.MkdirAll(filepath.Dir(schemaPath), 0o755); err != nil {
		return observation{}, fmt.Errorf("create schema directory: %w", err)
	}
	if err := os.WriteFile(schemaPath, []byte(current.schema), 0o600); err != nil {
		return observation{}, fmt.Errorf("write schema: %w", err)
	}
	finalDirectory := filepath.Join(root, "final")
	if err := os.MkdirAll(finalDirectory, 0o755); err != nil {
		return observation{}, fmt.Errorf("create final-output directory: %w", err)
	}
	finalFile, err := os.CreateTemp(finalDirectory, current.name+"-*.json")
	if err != nil {
		return observation{}, fmt.Errorf("create final-output target: %w", err)
	}
	finalPath := finalFile.Name()
	if err := finalFile.Close(); err != nil {
		return observation{}, fmt.Errorf("close final-output target: %w", err)
	}

	arguments := []string{
		binary,
		"exec",
		"--json",
		"--color", "never",
		"--sandbox", current.sandbox,
		"--output-schema", schemaPath,
		"--cd", workspace,
		"--output-last-message", finalPath,
		"-",
	}
	timeout := current.timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	result, runErr := processrun.NewRunner().Run(ctx, processrun.Request{
		Directory:        workspace,
		Argv:             arguments,
		Stdin:            []byte(current.prompt),
		Environment:      environment,
		CleanEnvironment: true,
		StdoutLimit:      outputLimit,
		StderrLimit:      1 << 20,
	})
	cancel()
	if result.StdoutTruncated || result.StderrTruncated {
		return observation{}, errors.New("captured output exceeded the harness limit")
	}

	sanitizedStdout := redact(result.Stdout)
	sanitizedStderr := redact(result.Stderr)
	eventTypes, sessionID, largestEvent, err := inspectJSONL(sanitizedStdout)
	if err != nil {
		return observation{}, fmt.Errorf("inspect JSONL: %w", err)
	}
	finalOutput, err := os.ReadFile(finalPath)
	if err != nil {
		return observation{}, fmt.Errorf("read final output: %w", err)
	}
	finalOutput = redact(finalOutput)
	trimmedFinalOutput := bytes.TrimSpace(finalOutput)
	if len(trimmedFinalOutput) > 0 && !json.Valid(trimmedFinalOutput) {
		return observation{}, errors.New("final output is not valid JSON")
	}

	scenarioEvidence := filepath.Join(evidenceRoot, current.name)
	if err := os.MkdirAll(scenarioEvidence, 0o755); err != nil {
		return observation{}, fmt.Errorf("create scenario evidence directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(scenarioEvidence, "events.jsonl"), sanitizedStdout, 0o644); err != nil {
		return observation{}, fmt.Errorf("write JSONL evidence: %w", err)
	}
	if err := os.WriteFile(filepath.Join(scenarioEvidence, "stderr.txt"), sanitizedStderr, 0o644); err != nil {
		return observation{}, fmt.Errorf("write stderr evidence: %w", err)
	}
	if len(trimmedFinalOutput) > 0 {
		if err := os.WriteFile(filepath.Join(scenarioEvidence, "final-output.json"), redact(finalOutput), 0o644); err != nil {
			return observation{}, fmt.Errorf("write final-output evidence: %w", err)
		}
	}

	outcome := "completed"
	if runErr != nil {
		outcome = "failed"
		if errors.Is(runErr, context.DeadlineExceeded) {
			outcome = "canceled"
		}
	}
	observed := observation{
		Name:                   current.name,
		Arguments:              sanitizedArguments(arguments, binary, schemaPath, workspace, finalPath),
		WorkingDirectory:       "$WORKSPACE",
		EnvironmentKeys:        sortedKeys(environment),
		Sandbox:                current.sandbox,
		ExitCode:               result.ExitCode,
		Outcome:                outcome,
		SessionID:              sessionID,
		EventTypes:             eventTypes,
		StdoutBytes:            len(sanitizedStdout),
		StderrBytes:            len(sanitizedStderr),
		LargestJSONLEventBytes: largestEvent,
		FinalOutputPresent:     len(trimmedFinalOutput) > 0,
		FinalOutputBytes:       len(finalOutput),
	}
	if runErr != nil {
		observed.Error = strings.TrimSpace(string(redact([]byte(runErr.Error()))))
	}
	if current.markerName != "" {
		marker, markerErr := os.ReadFile(filepath.Join(workspace, current.markerName))
		present := markerErr == nil
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return observation{}, fmt.Errorf("inspect workspace marker: %w", markerErr)
		}
		expected := current.markerText != ""
		matched := present && string(marker) == current.markerText
		observed.WorkspaceMarkerExpected = &expected
		observed.WorkspaceMarkerPresent = &present
		observed.WorkspaceMarkerMatched = &matched
	}
	if current.validate == nil {
		return observation{}, errors.New("scenario has no expected-outcome validator")
	}
	if err := current.validate(observed, runErr, result.Stdout); err != nil {
		return observation{}, err
	}
	if err := writeJSON(filepath.Join(scenarioEvidence, "observation.json"), observed); err != nil {
		return observation{}, err
	}
	return observed, nil
}

func scenarios() []scenario {
	const validSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["status", "summary", "question"],
  "properties": {
    "status": {"type": "string", "enum": ["completed"]},
    "summary": {"type": "string"},
    "question": {"type": "string", "enum": [""]}
  }
}`
	withSecret := func(instruction string) string {
		return instruction + "\nDo not repeat this irrelevant sentinel: " + promptSecret + ".\n"
	}
	return []scenario{
		{
			name:     "successful-structured",
			sandbox:  "read-only",
			schema:   validSchema,
			prompt:   withSecret(`Do not use tools. Return status "completed", summary "structured contract verified", and an empty question.`),
			validate: validateAuthenticatedSuccess,
		},
		{
			name:    "schema-rejected",
			sandbox: "read-only",
			schema: `{
  "type": "object",
  "additionalProperties": false,
  "properties": {"value": {"type": "string"}}
}`,
			prompt:   withSecret(`Do not use tools. Return an object with value "schema rejection probe".`),
			validate: validateSchemaRejection,
		},
		{
			name:       "read-only",
			sandbox:    "read-only",
			schema:     validSchema,
			markerName: "read-only-marker.txt",
			prompt: withSecret(`Run exactly this shell command before answering; do not predict or simulate its result:

python3 -c 'from pathlib import Path; Path("read-only-marker.txt").write_text("should-not-exist")'

Report the actual command result by returning status "completed", a short summary, and an empty question.`),
			validate: validateReadOnly,
		},
		{
			name:       "workspace-write",
			sandbox:    "workspace-write",
			schema:     validSchema,
			markerName: "workspace-write-marker.txt",
			markerText: "workspace-write-ok",
			prompt:     withSecret(`Create workspace-write-marker.txt in the current working directory with the exact contents "workspace-write-ok" using a shell command. Verify the contents. Then return status "completed", a short summary, and an empty question.`),
			validate:   validateWorkspaceWrite,
		},
		{
			name:     "large-event",
			sandbox:  "read-only",
			schema:   validSchema,
			prompt:   withSecret(`Run exactly this shell command and inspect its complete output: python3 -c 'print("x" * 131072)'. Then return status "completed", a short summary, and an empty question.`),
			validate: validateAuthenticatedSuccess,
		},
		{
			name:     "cancellation",
			sandbox:  "read-only",
			schema:   validSchema,
			timeout:  5 * time.Second,
			prompt:   withSecret(`Run exactly this shell command and wait for it to finish: sleep 30. After it finishes, return status "completed", a short summary, and an empty question.`),
			validate: validateCancellation,
		},
	}
}

func validateAuthenticatedSuccess(observed observation, runErr error, _ []byte) error {
	if runErr != nil {
		return fmt.Errorf("required authenticated success did not complete: %w", runErr)
	}
	if observed.ExitCode != 0 || observed.SessionID == "" || !observed.FinalOutputPresent {
		return errors.New("required authenticated success did not produce exit zero, a session ID, and final output")
	}
	return nil
}

func validateSchemaRejection(observed observation, runErr error, stdout []byte) error {
	hasInvalidSchema := bytes.Contains(stdout, []byte("invalid_json_schema"))
	if runErr == nil || observed.ExitCode == 0 || observed.FinalOutputPresent || !hasInvalidSchema {
		return fmt.Errorf("schema-rejected probe did not produce the expected invalid-schema failure: error=%v exit=%d final=%t event_types=%v invalid_schema=%t", runErr, observed.ExitCode, observed.FinalOutputPresent, observed.EventTypes, hasInvalidSchema)
	}
	return nil
}

func validateCancellation(observed observation, runErr error, _ []byte) error {
	if !errors.Is(runErr, context.DeadlineExceeded) || observed.FinalOutputPresent {
		return errors.New("cancellation probe did not produce the expected deadline failure")
	}
	return nil
}

func validateReadOnly(observed observation, runErr error, stdout []byte) error {
	if err := validateAuthenticatedSuccess(observed, runErr, stdout); err != nil {
		return err
	}
	if !bytes.Contains(stdout, []byte(`"type":"command_execution"`)) {
		return errors.New("read-only probe did not execute a shell command")
	}
	if observed.WorkspaceMarkerPresent == nil || *observed.WorkspaceMarkerPresent {
		return errors.New("read-only probe modified the workspace")
	}
	return nil
}

func validateWorkspaceWrite(observed observation, runErr error, stdout []byte) error {
	if err := validateAuthenticatedSuccess(observed, runErr, stdout); err != nil {
		return err
	}
	if observed.WorkspaceMarkerMatched == nil || !*observed.WorkspaceMarkerMatched {
		return errors.New("workspace-write probe did not write the expected marker")
	}
	return nil
}

func inspectJSONL(contents []byte) ([]string, string, int, error) {
	types := []string{}
	seenTypes := map[string]bool{}
	var sessionID string
	var largest int
	for number, line := range bytes.Split(contents, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > largest {
			largest = len(line)
		}
		var header struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return nil, "", 0, fmt.Errorf("line %d: %w", number+1, err)
		}
		if strings.TrimSpace(header.Type) == "" {
			return nil, "", 0, fmt.Errorf("line %d has no event type", number+1)
		}
		if !seenTypes[header.Type] {
			types = append(types, header.Type)
			seenTypes[header.Type] = true
		}
		if header.Type == "thread.started" {
			sessionID = header.ThreadID
		}
	}
	return types, sessionID, largest, nil
}

func contractEnvironment() map[string]string {
	environment := map[string]string{"AWDEV_WORKER": "1"}
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_HOME", "PATH"} {
		if value := os.Getenv(name); value != "" {
			environment[name] = value
		}
	}
	if environment["CODEX_HOME"] == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			environment["CODEX_HOME"] = filepath.Join(home, ".codex")
		}
	}
	return environment
}

func newRedactor(root, binary string) func([]byte) []byte {
	replacements := [][2]string{
		{root, "$SPIKE_ROOT"},
		{binary, "$CODEX"},
		{promptSecret, "[PROMPT_REDACTED]"},
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		replacements = append(replacements, [2]string{home, "$HOME"})
	}
	return func(contents []byte) []byte {
		clean := string(contents)
		for _, replacement := range replacements {
			clean = strings.ReplaceAll(clean, replacement[0], replacement[1])
		}
		clean = credentialPattern.ReplaceAllString(clean, "[CREDENTIAL_REDACTED]")
		clean = assignmentPattern.ReplaceAllString(clean, "${1}[REDACTED]")
		return []byte(clean)
	}
}

func sanitizedArguments(arguments []string, binary, schema, workspace, final string) []string {
	replacements := map[string]string{
		binary:    "$CODEX",
		schema:    "$SCHEMA",
		workspace: "$WORKSPACE",
		final:     "$FINAL_OUTPUT",
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
