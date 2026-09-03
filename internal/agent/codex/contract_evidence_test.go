package codex_test

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
)

type contractManifest struct {
	CodexVersion string                `json:"codex_version"`
	Platform     string                `json:"platform"`
	Scenarios    []contractObservation `json:"scenarios"`
}

type contractObservation struct {
	Name                    string   `json:"name"`
	Arguments               []string `json:"arguments"`
	WorkingDirectory        string   `json:"working_directory"`
	EnvironmentKeys         []string `json:"environment_keys"`
	Sandbox                 string   `json:"sandbox"`
	ExitCode                int      `json:"exit_code"`
	Outcome                 string   `json:"outcome"`
	Error                   string   `json:"error"`
	SessionID               string   `json:"session_id"`
	EventTypes              []string `json:"event_types"`
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
	if manifest.CodexVersion == "" || manifest.Platform == "" {
		t.Fatalf("manifest must identify the Codex version and platform: %#v", manifest)
	}

	seen := make(map[string]bool, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		seen[scenario.Name] = true
	}
	for _, name := range []string{
		"successful-structured",
		"schema-rejected",
		"read-only",
		"workspace-write",
		"large-event",
		"cancellation",
	} {
		if !seen[name] {
			t.Errorf("contract evidence does not include %q", name)
		}
	}
}

func TestRecordedContractEvidenceIsReplayable(t *testing.T) {
	t.Parallel()

	manifest := readContractManifest(t)
	for _, scenario := range manifest.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()

			directory := filepath.Join("testdata", "contract", scenario.Name)
			observationContents, err := os.ReadFile(filepath.Join(directory, "observation.json"))
			if err != nil {
				t.Fatalf("read observation: %v", err)
			}
			var recorded contractObservation
			if err := json.Unmarshal(observationContents, &recorded); err != nil {
				t.Fatalf("decode observation: %v", err)
			}
			if !reflect.DeepEqual(recorded, scenario) {
				t.Fatalf("scenario observation does not match manifest: file = %#v, manifest = %#v", recorded, scenario)
			}
			if recorded.Name != scenario.Name || recorded.WorkingDirectory != "$WORKSPACE" {
				t.Fatalf("observation = %#v", recorded)
			}
			if len(recorded.Arguments) == 0 || recorded.Arguments[0] != "$CODEX" || recorded.Arguments[len(recorded.Arguments)-1] != "-" {
				t.Fatalf("sanitized arguments = %#v", recorded.Arguments)
			}

			events, err := os.ReadFile(filepath.Join(directory, "events.jsonl"))
			if err != nil {
				t.Fatalf("read JSONL events: %v", err)
			}
			if len(events) != recorded.StdoutBytes {
				t.Errorf("recorded stdout bytes = %d, fixture = %d", recorded.StdoutBytes, len(events))
			}
			decoder := json.NewDecoder(bytes.NewReader(events))
			eventTypes := []string{}
			seenTypes := map[string]bool{}
			var sessionID string
			for {
				var event struct {
					Type     string `json:"type"`
					ThreadID string `json:"thread_id"`
				}
				if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("replay JSONL event: %v", err)
				}
				if strings.TrimSpace(event.Type) == "" {
					t.Fatal("replayed JSONL event has no type")
				}
				if !seenTypes[event.Type] {
					eventTypes = append(eventTypes, event.Type)
					seenTypes[event.Type] = true
				}
				if event.Type == "thread.started" {
					sessionID = event.ThreadID
				}
			}
			if !slices.Equal(eventTypes, recorded.EventTypes) || sessionID != recorded.SessionID {
				t.Errorf("replayed event types/session = %#v/%q, observation = %#v/%q", eventTypes, sessionID, recorded.EventTypes, recorded.SessionID)
			}
			largestEvent := 0
			for _, line := range bytes.Split(events, []byte{'\n'}) {
				line = bytes.TrimSpace(line)
				if len(line) > largestEvent {
					largestEvent = len(line)
				}
			}
			if largestEvent != recorded.LargestJSONLEventBytes {
				t.Errorf("replayed largest event = %d, observation = %d", largestEvent, recorded.LargestJSONLEventBytes)
			}
			stderr, err := os.ReadFile(filepath.Join(directory, "stderr.txt"))
			if err != nil {
				t.Fatalf("read stderr: %v", err)
			}
			if len(stderr) != recorded.StderrBytes {
				t.Errorf("recorded stderr bytes = %d, fixture = %d", recorded.StderrBytes, len(stderr))
			}

			finalPath := filepath.Join(directory, "final-output.json")
			finalOutput, err := os.ReadFile(finalPath)
			if scenario.FinalOutputPresent {
				if err != nil {
					t.Fatalf("read final output: %v", err)
				}
				if !json.Valid(finalOutput) {
					t.Fatal("final output is not valid JSON")
				}
				if len(finalOutput) != recorded.FinalOutputBytes {
					t.Errorf("recorded final-output bytes = %d, fixture = %d", recorded.FinalOutputBytes, len(finalOutput))
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("unexpected final output state: %v", err)
			}
		})
	}
}

func TestRecordedContractEvidenceMatchesObservedBoundaries(t *testing.T) {
	t.Parallel()

	manifest := readContractManifest(t)
	observations := make(map[string]contractObservation, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		observations[scenario.Name] = scenario
	}

	successful := observations["successful-structured"]
	if successful.ExitCode != 0 || successful.SessionID == "" || !successful.FinalOutputPresent {
		t.Errorf("successful structured run = %#v", successful)
	}
	rejected := observations["schema-rejected"]
	if rejected.ExitCode == 0 || rejected.FinalOutputPresent || !contains(rejected.EventTypes, "turn.failed") {
		t.Errorf("schema-rejected run = %#v", rejected)
	}
	readOnly := observations["read-only"]
	if readOnly.WorkspaceMarkerExpected == nil || *readOnly.WorkspaceMarkerExpected || readOnly.WorkspaceMarkerPresent == nil || *readOnly.WorkspaceMarkerPresent {
		t.Errorf("read-only run = %#v", readOnly)
	}
	readOnlyEvents, err := os.ReadFile(filepath.Join("testdata", "contract", "read-only", "events.jsonl"))
	if err != nil {
		t.Fatalf("read read-only events: %v", err)
	}
	if !bytes.Contains(readOnlyEvents, []byte(`"type":"command_execution"`)) {
		t.Error("read-only evidence does not contain an executed write probe")
	}
	workspaceWrite := observations["workspace-write"]
	if workspaceWrite.WorkspaceMarkerExpected == nil || !*workspaceWrite.WorkspaceMarkerExpected || workspaceWrite.WorkspaceMarkerMatched == nil || !*workspaceWrite.WorkspaceMarkerMatched {
		t.Errorf("workspace-write run = %#v", workspaceWrite)
	}
	large := observations["large-event"]
	if large.LargestJSONLEventBytes <= 64*1024 {
		t.Errorf("largest JSONL event = %d, want more than 64 KiB", large.LargestJSONLEventBytes)
	}
	canceled := observations["cancellation"]
	if canceled.Outcome != "canceled" || canceled.FinalOutputPresent {
		t.Errorf("canceled run = %#v", canceled)
	}
}

func TestRecordedContractEvidenceIsSanitized(t *testing.T) {
	t.Parallel()

	patterns := map[string]*regexp.Regexp{
		"absolute user path":    regexp.MustCompile(`(?i)(/Users/|/home/|[a-z]:\\Users\\)`),
		"credential prefix":     regexp.MustCompile(`(?i)\b(sk-|sess-|ghp_|github_pat_|xoxb-)[a-z0-9_-]{8,}`),
		"credential assignment": regexp.MustCompile(`(?i)(OPENAI_API_KEY|CODEX_API_KEY|CODEX_ACCESS_TOKEN)\s*[=:]`),
		"prompt sentinel":       regexp.MustCompile(`AWDEV_SPIKE_PROMPT_SECRET`),
	}
	root := filepath.Join("testdata", "contract")
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
				t.Errorf("%s contains %s", path, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan contract evidence: %v", err)
	}
}

func readContractManifest(t *testing.T) contractManifest {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "contract", "manifest.json"))
	if err != nil {
		t.Fatalf("read contract evidence manifest: %v", err)
	}
	var manifest contractManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatalf("decode contract evidence manifest: %v", err)
	}
	return manifest
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
