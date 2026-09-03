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
	"strings"

	"github.com/agenticworkflowdev/cli/internal/agent"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const (
	jsonlOutputLimit  = 32 << 20
	stderrOutputLimit = 1 << 20
)

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

// Run starts one fresh Codex exec session and reads its final result from the
// controller-owned output file.
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

	arguments := []string{
		runner.binary,
		"exec",
		"--json",
		"--sandbox", string(request.Access),
		"--output-schema", request.OutputSchema,
		"--cd", request.Worktree,
		"--output-last-message", outputPath,
		"-",
	}
	processResult, err := runner.process.Run(ctx, processrun.Request{
		Directory:        request.Worktree,
		Argv:             arguments,
		Stdin:            []byte(request.Prompt),
		Environment:      authenticationEnvironment(),
		CleanEnvironment: true,
		StdoutLimit:      jsonlOutputLimit,
		StderrLimit:      stderrOutputLimit,
	})
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("run Codex: %w", err)
	}
	if processResult.StdoutTruncated {
		return agent.RunResult{}, errors.New("Codex JSONL progress exceeded the output limit")
	}
	progress, sessionID, err := parseProgress(processResult.Stdout)
	if err != nil {
		return agent.RunResult{}, err
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
	info, err := os.Lstat(request.OutputDirectory)
	if err != nil {
		return fmt.Errorf("inspect Codex output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Codex output directory must be a real directory")
	}
	return nil
}

func authenticationEnvironment() map[string]string {
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

func parseProgress(contents []byte) ([]agent.ProgressEvent, string, error) {
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
		var header struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
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
		progress = append(progress, agent.ProgressEvent{Type: header.Type})
	}
	if sessionID == "" {
		return nil, "", errors.New("Codex session identity is missing")
	}
	return progress, sessionID, nil
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
