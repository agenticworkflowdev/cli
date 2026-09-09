package claudecode_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent/claudecode"
)

type contractManifest struct {
	RecordedAt      string                `json:"recorded_at"`
	ClaudeVersion   string                `json:"claude_version"`
	Platform        string                `json:"platform"`
	Invocation      []string              `json:"invocation"`
	EnvironmentKeys []string              `json:"environment_keys"`
	Scenarios       []contractObservation `json:"scenarios"`
}

type contractObservation struct {
	Name                    string   `json:"name"`
	Expectation             string   `json:"expectation"`
	Arguments               []string `json:"arguments"`
	WorkingDirectory        string   `json:"working_directory"`
	EnvironmentKeys         []string `json:"environment_keys"`
	PermissionMode          string   `json:"permission_mode"`
	ExitCode                int      `json:"exit_code"`
	Outcome                 string   `json:"outcome"`
	Error                   string   `json:"error"`
	SessionID               string   `json:"session_id"`
	SessionIDConsistent     bool     `json:"session_id_consistent"`
	EventTypes              []string `json:"event_types"`
	SystemSubtypes          []string `json:"system_subtypes"`
	ResultEventPresent      bool     `json:"result_event_present"`
	ResultIsError           bool     `json:"result_is_error"`
	ResultSubtype           string   `json:"result_subtype"`
	ResultTerminalReason    string   `json:"result_terminal_reason"`
	StructuredOutputPresent bool     `json:"structured_output_present"`
	StructuredOutputStatus  string   `json:"structured_output_status"`
	PermissionDenials       int      `json:"permission_denials"`
	StdoutBytes             int      `json:"stdout_bytes"`
	StderrBytes             int      `json:"stderr_bytes"`
	LargestJSONLEventBytes  int      `json:"largest_jsonl_event_bytes"`
	FinalOutputPresent      bool     `json:"final_output_present"`
	FinalOutputBytes        int      `json:"final_output_bytes"`
	WorkspaceMarkerExpected *bool    `json:"workspace_marker_expected"`
	WorkspaceMarkerPresent  *bool    `json:"workspace_marker_present"`
	WorkspaceMarkerMatched  *bool    `json:"workspace_marker_matched"`
}

func TestRecordedContractEvidenceCoversRequiredScenarios(t *testing.T) {
	t.Parallel()

	manifest := readContractManifest(t)
	if manifest.ClaudeVersion == "" || manifest.Platform == "" {
		t.Fatalf("manifest must identify the Claude Code version and platform: %#v", manifest)
	}
	if len(manifest.Invocation) == 0 || manifest.Invocation[0] != "$CLAUDE" {
		t.Fatalf("manifest invocation is not sanitized: %#v", manifest.Invocation)
	}

	seen := make(map[string]bool, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		seen[scenario.Name] = true
	}
	for _, name := range []string{
		"successful-structured",
		"read-only",
		"workspace-write",
		"schema-rejected",
		"blocked",
		"cancellation",
		"large-event",
	} {
		if !seen[name] {
			t.Errorf("contract evidence does not include %q", name)
		}
	}
}

func TestRecordedContractEvidenceIsReplayableThroughTheParser(t *testing.T) {
	t.Parallel()

	manifest := readContractManifest(t)
	for _, scenario := range manifest.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()

			directory := filepath.Join("testdata", "contract", scenario.Name)
			var recorded contractObservation
			readJSON(t, filepath.Join(directory, "observation.json"), &recorded)
			if !reflect.DeepEqual(recorded, scenario) {
				t.Fatalf("observation does not match manifest:\nfile     = %#v\nmanifest = %#v", recorded, scenario)
			}
			if len(recorded.Arguments) == 0 || recorded.Arguments[0] != "$CLAUDE" {
				t.Fatalf("sanitized arguments = %#v", recorded.Arguments)
			}
			if recorded.WorkingDirectory != "$WORKSPACE" {
				t.Fatalf("working directory = %q", recorded.WorkingDirectory)
			}
			if !slices.Contains(recorded.Arguments, "--print") ||
				!slices.Contains(recorded.Arguments, "stream-json") ||
				!slices.Contains(recorded.Arguments, "--verbose") {
				t.Fatalf("arguments miss the verified contract flags: %#v", recorded.Arguments)
			}
			if got := recorded.Arguments[len(recorded.Arguments)-2:]; !slices.Equal(got, []string{"--add-dir", "$WORKSPACE"}) {
				t.Fatalf("arguments do not end with the worktree grant: %#v", got)
			}

			events := readFileBytes(t, filepath.Join(directory, "events.jsonl"))
			if len(events) != recorded.StdoutBytes {
				t.Errorf("recorded stdout bytes = %d, fixture = %d", recorded.StdoutBytes, len(events))
			}
			stderr := readFileBytes(t, filepath.Join(directory, "stderr.txt"))
			if len(stderr) != recorded.StderrBytes {
				t.Errorf("recorded stderr bytes = %d, fixture = %d", recorded.StderrBytes, len(stderr))
			}

			largestLine := 0
			for _, line := range bytes.Split(events, []byte{'\n'}) {
				if trimmed := len(bytes.TrimSpace(line)); trimmed > largestLine {
					largestLine = trimmed
				}
			}
			if largestLine != recorded.LargestJSONLEventBytes {
				t.Errorf("largest event = %d, observation = %d", largestLine, recorded.LargestJSONLEventBytes)
			}

			decoder := json.NewDecoder(bytes.NewReader(events))
			var eventTypes []string
			seenType := map[string]bool{}
			for {
				var event struct {
					Type string `json:"type"`
				}
				if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("decode recorded event: %v", err)
				}
				if event.Type == "" {
					t.Fatal("recorded event has no type")
				}
				if !seenType[event.Type] {
					seenType[event.Type] = true
					eventTypes = append(eventTypes, event.Type)
				}
			}
			if !slices.Equal(eventTypes, recorded.EventTypes) {
				t.Errorf("event types = %#v, observation = %#v", eventTypes, recorded.EventTypes)
			}

			finalPath := filepath.Join(directory, "final-output.json")
			finalOutput, finalErr := os.ReadFile(finalPath)
			if recorded.FinalOutputPresent {
				if finalErr != nil {
					t.Fatalf("read final output: %v", finalErr)
				}
				if !json.Valid(finalOutput) {
					t.Fatal("final output is not valid JSON")
				}
				if len(finalOutput) != recorded.FinalOutputBytes {
					t.Errorf("recorded final-output bytes = %d, fixture = %d", recorded.FinalOutputBytes, len(finalOutput))
				}
			} else if !os.IsNotExist(finalErr) {
				t.Fatalf("unexpected final-output state: %v", finalErr)
			}

			if len(events) == 0 {
				return
			}

			replay, err := claudecode.ReplayStream(events)
			if err != nil {
				t.Fatalf("replay recorded stream through the parser: %v", err)
			}
			if replay.SessionID != recorded.SessionID {
				t.Errorf("replayed session id = %q, observation = %q", replay.SessionID, recorded.SessionID)
			}
			if replay.ResultPresent != recorded.ResultEventPresent {
				t.Errorf("replayed result present = %t, observation = %t", replay.ResultPresent, recorded.ResultEventPresent)
			}
			if replay.ResultIsError != recorded.ResultIsError {
				t.Errorf("replayed result is_error = %t, observation = %t", replay.ResultIsError, recorded.ResultIsError)
			}
			if replay.StructuredOutputPresent != recorded.StructuredOutputPresent {
				t.Errorf("replayed structured output present = %t, observation = %t", replay.StructuredOutputPresent, recorded.StructuredOutputPresent)
			}
			if replay.StructuredOutputPresent {
				if !json.Valid(replay.StructuredOutput) {
					t.Fatalf("replayed structured output is not valid JSON: %s", replay.StructuredOutput)
				}
				var fromStream, fromFile any
				if err := json.Unmarshal(replay.StructuredOutput, &fromStream); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(finalOutput, &fromFile); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(fromStream, fromFile) {
					t.Errorf("replayed structured output %v != recorded final output %v", fromStream, fromFile)
				}
			}
		})
	}
}

func TestRecordedContractEvidenceMatchesObservedBoundaries(t *testing.T) {
	t.Parallel()

	observations := map[string]contractObservation{}
	for _, scenario := range readContractManifest(t).Scenarios {
		observations[scenario.Name] = scenario
	}

	successful := observations["successful-structured"]
	if successful.ExitCode != 0 || successful.SessionID == "" || !successful.FinalOutputPresent || !successful.StructuredOutputPresent || successful.ResultIsError {
		t.Errorf("successful-structured = %#v", successful)
	}

	rejected := observations["schema-rejected"]
	if rejected.ExitCode == 0 || rejected.ResultEventPresent || rejected.FinalOutputPresent || rejected.StderrBytes == 0 {
		t.Errorf("schema-rejected = %#v", rejected)
	}

	readOnly := observations["read-only"]
	if readOnly.PermissionMode != "plan" || readOnly.PermissionDenials != 1 || !slices.Contains(readOnly.SystemSubtypes, "permission_denied") {
		t.Errorf("read-only = %#v", readOnly)
	}
	if readOnly.WorkspaceMarkerExpected == nil || *readOnly.WorkspaceMarkerExpected ||
		readOnly.WorkspaceMarkerPresent == nil || *readOnly.WorkspaceMarkerPresent {
		t.Errorf("read-only marker postconditions = %#v", readOnly)
	}

	workspaceWrite := observations["workspace-write"]
	if workspaceWrite.PermissionMode != "acceptEdits" || workspaceWrite.PermissionDenials != 0 {
		t.Errorf("workspace-write = %#v", workspaceWrite)
	}
	if workspaceWrite.WorkspaceMarkerExpected == nil || !*workspaceWrite.WorkspaceMarkerExpected ||
		workspaceWrite.WorkspaceMarkerMatched == nil || !*workspaceWrite.WorkspaceMarkerMatched {
		t.Errorf("workspace-write marker postconditions = %#v", workspaceWrite)
	}

	blocked := observations["blocked"]
	if blocked.ExitCode != 0 || blocked.ResultIsError || blocked.StructuredOutputStatus != "blocked" {
		t.Errorf("blocked = %#v", blocked)
	}

	cancellation := observations["cancellation"]
	if cancellation.Outcome != "canceled" || cancellation.ResultEventPresent || cancellation.FinalOutputPresent || cancellation.ExitCode != 143 {
		t.Errorf("cancellation = %#v", cancellation)
	}

	large := observations["large-event"]
	if large.LargestJSONLEventBytes <= 64*1024 {
		t.Errorf("large-event largest JSONL event = %d, want more than 64 KiB", large.LargestJSONLEventBytes)
	}
}

func TestRecordedContractEvidenceIsSanitized(t *testing.T) {
	t.Parallel()

	patterns := map[string]*regexp.Regexp{
		"absolute user path":    regexp.MustCompile(`(?i)(/Users/|/home/|/root/|[a-z]:\\Users\\)`),
		"credential prefix":     regexp.MustCompile(`(?i)\b(sk-ant-|sk-|sess-|ghp_|github_pat_|xox[bp]-)[A-Za-z0-9_-]{8,}`),
		"credential assignment": regexp.MustCompile(`(?i)(ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN)\s*[=:]`),
	}
	pathToken := regexp.MustCompile(`\$[A-Z_]+(/[A-Za-z0-9._-]+)+`)
	allowedTokens := map[string]bool{
		"$WORKSPACE": true, "$HOME": true, "$CONFIG_DIR": true, "$CLAUDE": true,
		"$SCHEMA": true, "$SESSION": true, "$RUNTIME": true,
	}

	root := filepath.Join("testdata", "contract")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
		for name, pattern := range patterns {
			if pattern.Match(contents) {
				t.Errorf("%s contains a %s", path, name)
			}
		}
		for _, match := range pathToken.FindAllString(string(contents), -1) {
			prefix := match
			if index := strings.IndexByte(match, '/'); index >= 0 {
				prefix = match[:index]
			}
			if !allowedTokens[prefix] {
				t.Errorf("%s contains an unredacted path token %q", path, match)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan contract evidence: %v", err)
	}
}

func readContractManifest(t *testing.T) contractManifest {
	t.Helper()
	var manifest contractManifest
	readJSON(t, filepath.Join("testdata", "contract", "manifest.json"), &manifest)
	return manifest
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	if err := json.Unmarshal(readFileBytes(t, path), target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return contents
}
