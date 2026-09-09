// Package claudecode adapts the Claude Code CLI to the provider-neutral agent contract.
package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/agenticworkflowdev/cli/internal/agent"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const (
	streamOutputLimit = 32 << 20
	stderrOutputLimit = 1 << 20
	liveEventLimit    = 4 << 20
)

var exitCodePattern = regexp.MustCompile(`Exit code (\d+)`)

// Runner invokes a configured Claude Code executable.
type Runner struct {
	binary  string
	model   string
	process processrun.Runner
}

// NewRunner constructs a Claude Code adapter. The model may be empty, in which
// case the CLI default is used.
func NewRunner(binary, model string, process processrun.Runner) (*Runner, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("Claude Code binary must not be empty")
	}
	if process == nil {
		return nil, errors.New("Claude Code process runner is required")
	}
	return &Runner{binary: binary, model: model, process: process}, nil
}

// Run starts one fresh Claude Code session and reads its authoritative result
// from the terminal event of the stream-json output.
func (runner *Runner) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	if runner == nil || runner.process == nil {
		return agent.RunResult{}, errors.New("Claude Code runner is not configured")
	}
	if err := validateRequest(request); err != nil {
		return agent.RunResult{}, err
	}

	rawSchema, err := os.ReadFile(request.OutputSchema)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("read Claude Code output schema: %w", err)
	}
	schema, err := structuredOutputSchema(rawSchema)
	if err != nil {
		return agent.RunResult{}, err
	}
	sessionID, err := newSessionIdentity()
	if err != nil {
		return agent.RunResult{}, err
	}

	permissionMode := "acceptEdits"
	if request.Access == agent.AccessReadOnly {
		permissionMode = "plan"
	}

	arguments := []string{
		runner.binary,
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--json-schema", schema,
		"--permission-mode", permissionMode,
		"--permission-prompts", "none",
	}
	if strings.TrimSpace(runner.model) != "" {
		arguments = append(arguments, "--model", runner.model)
	}
	arguments = append(arguments, "--session-id", sessionID, "--add-dir", request.Worktree)

	environment, sensitiveValues := authenticationEnvironment()
	liveProgress := newLiveProgressWriter(request.Progress, sensitiveValues)
	processResult, runErr := runner.process.Run(ctx, processrun.Request{
		Directory:        request.Worktree,
		Argv:             arguments,
		Stdin:            []byte(request.Prompt),
		Environment:      environment,
		CleanEnvironment: true,
		StdoutLimit:      streamOutputLimit,
		StderrLimit:      stderrOutputLimit,
		StdoutObserver:   liveProgress.Observe,
	})
	liveProgress.Close()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return agent.RunResult{}, fmt.Errorf("run Claude Code: %w", ctxErr)
	}
	if runErr != nil {
		if stderr := firstLine(processResult.Stderr); stderr != "" {
			return agent.RunResult{}, fmt.Errorf("run Claude Code: %s", stderr)
		}
		return agent.RunResult{}, fmt.Errorf("run Claude Code: %w", runErr)
	}
	if processResult.StdoutTruncated {
		return agent.RunResult{}, errors.New("Claude Code stream exceeded the output limit")
	}

	stream, err := parseStream(processResult.Stdout, sensitiveValues)
	if err != nil {
		return agent.RunResult{}, err
	}
	if stream.result == nil {
		return agent.RunResult{}, errors.New("Claude Code stream ended without a result event")
	}
	if stream.result.isError {
		message := strings.TrimSpace(stream.result.resultText)
		if message == "" {
			message = "Claude Code reported an error"
		}
		if stream.result.apiErrorStatus != "" {
			return agent.RunResult{}, fmt.Errorf("Claude Code reported an error (api_error_status %s): %s", stream.result.apiErrorStatus, message)
		}
		return agent.RunResult{}, fmt.Errorf("Claude Code reported an error: %s", message)
	}
	if len(stream.result.structuredOutput) == 0 {
		return agent.RunResult{}, errors.New("Claude Code completed without a structured result: the model answered with prose instead of the structured output tool")
	}
	return agent.RunResult{
		FinalOutput: stream.result.structuredOutput,
		SessionID:   stream.sessionID,
		Progress:    stream.progress,
	}, nil
}

func validateRequest(request agent.Request) error {
	for name, value := range map[string]string{
		"worktree":         request.Worktree,
		"output schema":    request.OutputSchema,
		"output directory": request.OutputDirectory,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("Claude Code %s must be an absolute clean path", name)
		}
	}
	if request.Prompt == "" {
		return errors.New("Claude Code prompt must not be empty")
	}
	if request.Access != agent.AccessReadOnly && request.Access != agent.AccessWorkspaceWrite {
		return fmt.Errorf("unsupported Claude Code access level %q", request.Access)
	}
	schemaInfo, err := os.Lstat(request.OutputSchema)
	if err != nil {
		return fmt.Errorf("inspect Claude Code output schema: %w", err)
	}
	if schemaInfo.Mode()&os.ModeSymlink != 0 || !schemaInfo.Mode().IsRegular() {
		return errors.New("Claude Code output schema must be a regular file")
	}
	info, err := os.Lstat(request.OutputDirectory)
	if err != nil {
		return fmt.Errorf("inspect Claude Code output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Claude Code output directory must be a real directory")
	}
	return nil
}

// structuredOutputSchema adapts a repository-owned JSON Schema to what Claude
// Code's --json-schema flag accepts. Claude Code validates the object shape
// only and does not resolve remote meta-schema references, so a "$schema"
// dialect URI (and the informational "$id") make it reject the whole schema.
func structuredOutputSchema(raw []byte) (string, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", fmt.Errorf("parse Claude Code output schema: %w", err)
	}
	delete(document, "$schema")
	delete(document, "$id")
	adapted, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode Claude Code output schema: %w", err)
	}
	return string(adapted), nil
}

func authenticationEnvironment() (map[string]string, []string) {
	environment := map[string]string{"AWDEV_WORKER": "1"}
	sensitiveValues := make([]string, 0, 3)
	for _, name := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"CLAUDE_CONFIG_DIR",
		"HOME",
		"PATH",
	} {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		environment[name] = value
		switch name {
		case "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN":
			sensitiveValues = append(sensitiveValues, value)
		}
	}
	if environment["CLAUDE_CONFIG_DIR"] == "" {
		home := environment["HOME"]
		if home == "" {
			if resolved, err := os.UserHomeDir(); err == nil {
				home = resolved
			}
		}
		if home != "" {
			environment["CLAUDE_CONFIG_DIR"] = filepath.Join(home, ".claude")
		}
	}
	sort.Slice(sensitiveValues, func(left, right int) bool { return len(sensitiveValues[left]) > len(sensitiveValues[right]) })
	return environment, sensitiveValues
}

func newSessionIdentity() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", fmt.Errorf("generate Claude Code session identity: %w", err)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16]), nil
}

func firstLine(contents []byte) string {
	text := strings.TrimSpace(string(contents))
	if text == "" {
		return ""
	}
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return text
}

type streamOutcome struct {
	progress  []agent.ProgressEvent
	sessionID string
	result    *finalResult
}

type finalResult struct {
	isError          bool
	apiErrorStatus   string
	structuredOutput json.RawMessage
	resultText       string
	terminalReason   string
}

type streamEvent struct {
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype"`
	Message       json.RawMessage `json:"message"`
	ToolUseResult json.RawMessage `json:"tool_use_result"`
}

type streamContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    struct {
		Command string `json:"command"`
	} `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// contentBlocks extracts the assistant/user content blocks, tolerating events
// whose "message" field is a bare string (for example system/permission_denied).
func contentBlocks(message json.RawMessage) []streamContentBlock {
	trimmed := bytes.TrimSpace(message)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var payload struct {
		Content []streamContentBlock `json:"content"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil
	}
	return payload.Content
}

func parseStream(contents []byte, sensitiveValues []string) (streamOutcome, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	outcome := streamOutcome{progress: []agent.ProgressEvent{}}
	toolNames := map[string]string{}
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return streamOutcome{}, fmt.Errorf("decode Claude Code stream: %w", err)
		}
		var header struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return streamOutcome{}, fmt.Errorf("decode Claude Code stream event: %w", err)
		}
		if strings.TrimSpace(header.Type) == "" {
			return streamOutcome{}, errors.New("Claude Code stream event type is missing")
		}
		if session := strings.TrimSpace(header.SessionID); session != "" {
			if outcome.sessionID != "" && outcome.sessionID != session {
				return streamOutcome{}, errors.New("Claude Code stream contains conflicting session identities")
			}
			outcome.sessionID = session
		}

		var event streamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return streamOutcome{}, fmt.Errorf("decode Claude Code stream event: %w", err)
		}
		for _, progress := range eventProgress(event, toolNames) {
			outcome.progress = append(outcome.progress, redactProgressEvent(progress, sensitiveValues))
		}
		if header.Type == "result" {
			final, err := parseFinalResult(raw)
			if err != nil {
				return streamOutcome{}, err
			}
			outcome.result = final
		}
	}
	if outcome.sessionID == "" {
		return streamOutcome{}, errors.New("Claude Code session identity is missing")
	}
	return outcome, nil
}

func parseFinalResult(raw json.RawMessage) (*finalResult, error) {
	var payload struct {
		IsError          bool            `json:"is_error"`
		APIErrorStatus   json.RawMessage `json:"api_error_status"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Result           string          `json:"result"`
		TerminalReason   string          `json:"terminal_reason"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode Claude Code result event: %w", err)
	}
	final := &finalResult{
		isError:        payload.IsError,
		resultText:     payload.Result,
		terminalReason: payload.TerminalReason,
	}
	if status := strings.TrimSpace(string(payload.APIErrorStatus)); status != "" && status != "null" {
		final.apiErrorStatus = strings.Trim(status, `"`)
	}
	if trimmed := bytes.TrimSpace(payload.StructuredOutput); len(trimmed) > 0 && string(trimmed) != "null" {
		compacted := &bytes.Buffer{}
		if err := json.Compact(compacted, trimmed); err != nil {
			return nil, fmt.Errorf("compact Claude Code structured output: %w", err)
		}
		final.structuredOutput = append(json.RawMessage(nil), compacted.Bytes()...)
	}
	return final, nil
}

func eventProgress(event streamEvent, toolNames map[string]string) []agent.ProgressEvent {
	switch event.Type {
	case "assistant":
		blocks := contentBlocks(event.Message)
		events := make([]agent.ProgressEvent, 0, len(blocks))
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) != "" {
					events = append(events, agent.ProgressEvent{Type: "assistant", Kind: agent.ProgressMessage, Message: block.Text})
				}
			case "thinking":
				if strings.TrimSpace(block.Thinking) != "" {
					events = append(events, agent.ProgressEvent{Type: "assistant", Kind: agent.ProgressReasoning, Message: block.Thinking})
				}
			case "tool_use":
				if block.ID != "" && block.Name != "" {
					toolNames[block.ID] = block.Name
				}
				if block.Name == "Bash" {
					events = append(events, agent.ProgressEvent{Type: "assistant", Kind: agent.ProgressCommand, Message: block.Input.Command})
				}
			}
		}
		if len(events) == 0 {
			return []agent.ProgressEvent{{Type: "assistant"}}
		}
		return events
	case "user":
		blocks := contentBlocks(event.Message)
		events := make([]agent.ProgressEvent, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "tool_result" || toolNames[block.ToolUseID] != "Bash" {
				continue
			}
			message, exitCode := commandOutput(event.ToolUseResult, block.Content)
			events = append(events, agent.ProgressEvent{Type: "user", Kind: agent.ProgressCommandOutput, Message: message, ExitCode: exitCode})
		}
		if len(events) == 0 {
			return []agent.ProgressEvent{{Type: "user"}}
		}
		return events
	case "system":
		if subtype := strings.TrimSpace(event.Subtype); subtype != "" {
			return []agent.ProgressEvent{{Type: "system/" + subtype}}
		}
		return []agent.ProgressEvent{{Type: "system"}}
	default:
		return []agent.ProgressEvent{{Type: event.Type}}
	}
}

func commandOutput(toolUseResult json.RawMessage, blockContent json.RawMessage) (string, *int) {
	var message string
	trimmed := bytes.TrimSpace(toolUseResult)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '{':
		var structured struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		}
		if err := json.Unmarshal(trimmed, &structured); err == nil {
			message = structured.Stdout
			if structured.Stderr != "" {
				if message != "" {
					message += "\n"
				}
				message += structured.Stderr
			}
		}
	case len(trimmed) > 0 && trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			message = text
		}
	}
	var blockText string
	if content := bytes.TrimSpace(blockContent); len(content) > 0 && content[0] == '"' {
		_ = json.Unmarshal(content, &blockText)
	}
	if message == "" {
		message = blockText
	}
	return message, parseExitCode(message, blockText, string(trimmed))
}

func parseExitCode(texts ...string) *int {
	for _, text := range texts {
		matches := exitCodePattern.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			continue
		}
		if value, err := strconv.Atoi(matches[len(matches)-1][1]); err == nil {
			return &value
		}
	}
	return nil
}

func redactProgressEvent(event agent.ProgressEvent, sensitiveValues []string) agent.ProgressEvent {
	for _, sensitiveValue := range sensitiveValues {
		if sensitiveValue == "" {
			continue
		}
		event.Message = strings.ReplaceAll(event.Message, sensitiveValue, "[REDACTED]")
	}
	return event
}

type liveProgressWriter struct {
	report          func(agent.ProgressEvent)
	sensitiveValues []string
	toolNames       map[string]string
	mutex           sync.Mutex
	pending         []byte
	dropping        bool
	closed          bool
}

func newLiveProgressWriter(report func(agent.ProgressEvent), sensitiveValues []string) *liveProgressWriter {
	return &liveProgressWriter{
		report:          report,
		sensitiveValues: append([]string(nil), sensitiveValues...),
		toolNames:       map[string]string{},
	}
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
	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}
	if strings.TrimSpace(event.Type) == "" {
		return
	}
	for _, progress := range eventProgress(event, writer.toolNames) {
		if progress.Kind == "" {
			continue
		}
		writer.report(redactProgressEvent(progress, writer.sensitiveValues))
	}
}
