// Package codex adapts the Codex CLI to the provider-neutral agent contract.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/agenticworkflowdev/cli/internal/agent"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const (
	jsonlOutputLimit  = 32 << 20
	stderrOutputLimit = 1 << 20
	liveEventLimit    = 1 << 20
)

type progressEnvelope struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
	} `json:"item"`
}

// Runner invokes a configured Codex executable.
type Runner struct {
	binary  string
	process processrun.Runner
}

// NewRunner constructs a Codex adapter.
func NewRunner(binary string, process processrun.Runner) (*Runner, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("Codex binary must not be empty")
	}
	if process == nil {
		return nil, errors.New("Codex process runner is required")
	}
	return &Runner{binary: binary, process: process}, nil
}

// Provider reports the durable session namespace owned by this adapter.
func (*Runner) Provider() agent.Provider {
	return agent.ProviderCodex
}

// Run starts or resumes one Codex exec session and reads its final result from
// the controller-owned output file.
func (runner *Runner) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	if runner == nil || runner.process == nil {
		return agent.RunResult{}, errors.New("Codex runner is not configured")
	}
	if err := validateRequest(request); err != nil {
		return agent.RunResult{}, err
	}

	output, err := os.CreateTemp(request.OutputDirectory, ".agent-final-*.json")
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("create Codex final output: %w", err)
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return agent.RunResult{}, fmt.Errorf("close Codex final output: %w", err)
	}
	defer os.Remove(outputPath)

	arguments := []string{runner.binary, "exec"}
	if request.ResumeSessionID == "" {
		arguments = append(arguments,
			"--json",
			"--sandbox", string(request.Access),
			"--output-schema", request.OutputSchema,
			"--cd", request.Worktree,
			"--output-last-message", outputPath,
			"-",
		)
	} else {
		arguments = append(arguments,
			"--json",
			"--sandbox", string(request.Access),
			"--cd", request.Worktree,
			"--output-schema", request.OutputSchema,
			"--output-last-message", outputPath,
			"resume",
			request.ResumeSessionID,
			"-",
		)
	}
	environment, sensitiveValues := authenticationEnvironment()
	liveProgress := newLiveProgressWriter(request.Progress, sensitiveValues)
	processResult, err := runner.process.Run(ctx, processrun.Request{
		Directory:        request.Worktree,
		Argv:             arguments,
		Stdin:            []byte(request.Prompt),
		Environment:      environment,
		CleanEnvironment: true,
		StdoutLimit:      jsonlOutputLimit,
		StderrLimit:      stderrOutputLimit,
		StdoutObserver:   liveProgress.Observe,
	})
	liveProgress.Close()
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("run Codex: %w", err)
	}
	if processResult.StdoutTruncated {
		return agent.RunResult{}, errors.New("Codex JSONL progress exceeded the output limit")
	}
	progress, sessionID, err := parseProgress(processResult.Stdout, sensitiveValues)
	if err != nil {
		return agent.RunResult{}, err
	}
	if !agent.ValidSessionID(sessionID) {
		return agent.RunResult{}, errors.New("Codex reported a malformed session identity")
	}
	if request.ResumeSessionID != "" && sessionID != request.ResumeSessionID {
		return agent.RunResult{}, fmt.Errorf("Codex resumed session %q but reported %q", request.ResumeSessionID, sessionID)
	}
	finalOutput, err := readFinalOutput(outputPath)
	if err != nil {
		return agent.RunResult{}, err
	}
	return agent.RunResult{FinalOutput: finalOutput, SessionID: sessionID, Progress: progress}, nil
}

func validateRequest(request agent.Request) error {
	for name, value := range map[string]string{
		"worktree":         request.Worktree,
		"output schema":    request.OutputSchema,
		"output directory": request.OutputDirectory,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("Codex %s must be an absolute clean path", name)
		}
	}
	if request.Prompt == "" {
		return errors.New("Codex prompt must not be empty")
	}
	if request.Access != agent.AccessReadOnly && request.Access != agent.AccessWorkspaceWrite {
		return fmt.Errorf("unsupported Codex access level %q", request.Access)
	}
	if request.ResumeSessionID != "" && !agent.ValidSessionID(request.ResumeSessionID) {
		return errors.New("Codex resume session identity is malformed")
	}
	info, err := os.Lstat(request.OutputDirectory)
	if err != nil {
		return fmt.Errorf("inspect Codex output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Codex output directory must be a real directory")
	}
	return nil
}

func authenticationEnvironment() (map[string]string, []string) {
	environment := map[string]string{"AWDEV_WORKER": "1"}
	sensitiveValues := make([]string, 0, 3)
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_HOME", "PATH"} {
		if value := os.Getenv(name); value != "" {
			environment[name] = value
			if name != "CODEX_HOME" && name != "PATH" {
				sensitiveValues = append(sensitiveValues, value)
			}
		}
	}
	if environment["CODEX_HOME"] == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			environment["CODEX_HOME"] = filepath.Join(home, ".codex")
		}
	}
	sort.Slice(sensitiveValues, func(left, right int) bool { return len(sensitiveValues[left]) > len(sensitiveValues[right]) })
	return environment, sensitiveValues
}

func parseProgress(contents []byte, sensitiveValues []string) ([]agent.ProgressEvent, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	progress := []agent.ProgressEvent{}
	var sessionID string
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", fmt.Errorf("decode Codex JSONL progress: %w", err)
		}
		var header progressEnvelope
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, "", fmt.Errorf("decode Codex JSONL event: %w", err)
		}
		if strings.TrimSpace(header.Type) == "" {
			return nil, "", errors.New("Codex JSONL event type is missing")
		}
		if header.Type == "thread.started" {
			if strings.TrimSpace(header.ThreadID) == "" {
				return nil, "", errors.New("Codex session identity is missing")
			}
			if sessionID != "" && sessionID != header.ThreadID {
				return nil, "", errors.New("Codex JSONL contains conflicting session identities")
			}
			sessionID = header.ThreadID
		}
		progress = append(progress, redactProgressEvent(progressEvent(header), sensitiveValues))
	}
	if sessionID == "" {
		return nil, "", errors.New("Codex session identity is missing")
	}
	return progress, sessionID, nil
}

func redactProgressEvent(event agent.ProgressEvent, sensitiveValues []string) agent.ProgressEvent {
	for _, sensitiveValue := range sensitiveValues {
		event.Message = strings.ReplaceAll(event.Message, sensitiveValue, "[REDACTED]")
	}
	return event
}

func progressEvent(event progressEnvelope) agent.ProgressEvent {
	progress := agent.ProgressEvent{Type: event.Type}
	if event.Type != "item.started" && event.Type != "item.completed" {
		return progress
	}
	switch event.Item.Type {
	case "reasoning":
		if event.Type == "item.completed" {
			progress.Kind = agent.ProgressReasoning
			progress.Message = event.Item.Text
		}
	case "agent_message":
		if event.Type == "item.completed" {
			progress.Kind = agent.ProgressMessage
			progress.Message = event.Item.Text
		}
	case "command_execution":
		if event.Type == "item.started" {
			progress.Kind = agent.ProgressCommand
			progress.Message = event.Item.Command
		} else {
			progress.Kind = agent.ProgressCommandOutput
			progress.Message = event.Item.AggregatedOutput
			progress.ExitCode = event.Item.ExitCode
		}
	}
	return progress
}

type liveProgressWriter struct {
	report          func(agent.ProgressEvent)
	sensitiveValues []string
	mutex           sync.Mutex
	pending         []byte
	dropping        bool
	closed          bool
}

func newLiveProgressWriter(report func(agent.ProgressEvent), sensitiveValues []string) *liveProgressWriter {
	return &liveProgressWriter{report: report, sensitiveValues: append([]string(nil), sensitiveValues...)}
}

func (writer *liveProgressWriter) Observe(contents []byte) {
	if writer == nil || writer.report == nil {
		return
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.closed {
		return
	}
	for len(contents) > 0 {
		if writer.dropping {
			newline := bytes.IndexByte(contents, '\n')
			if newline < 0 {
				return
			}
			writer.dropping = false
			contents = contents[newline+1:]
			continue
		}

		newline := bytes.IndexByte(contents, '\n')
		if newline < 0 {
			if len(writer.pending)+len(contents) > liveEventLimit {
				writer.pending = nil
				writer.dropping = true
				return
			}
			writer.pending = append(writer.pending, contents...)
			return
		}
		if len(writer.pending)+newline <= liveEventLimit {
			writer.pending = append(writer.pending, contents[:newline]...)
			writer.emit()
		}
		writer.pending = nil
		contents = contents[newline+1:]
	}
}

func (writer *liveProgressWriter) Close() {
	if writer == nil || writer.report == nil {
		return
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.closed {
		return
	}
	if !writer.dropping && len(writer.pending) > 0 {
		writer.emit()
	}
	writer.pending = nil
	writer.closed = true
}

func (writer *liveProgressWriter) emit() {
	line := bytes.TrimSpace(writer.pending)
	if len(line) == 0 {
		return
	}
	var envelope progressEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	event := progressEvent(envelope)
	if event.Kind != "" {
		writer.report(redactProgressEvent(event, writer.sensitiveValues))
	}
}

func readFinalOutput(path string) (json.RawMessage, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex final output: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("Codex final output is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Codex final output: %w", err)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, errors.New("Codex final output is missing")
	}
	return append(json.RawMessage(nil), contents...), nil
}
