// Package checks executes repository-defined deterministic checks directly.
package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const outputLimit = 1 << 20

// Definition describes one configured deterministic check.
type Definition struct {
	Name      string
	Directory string
	Command   []string
	Timeout   time.Duration
}

// Result is the complete bounded evidence from one check invocation.
type Result struct {
	Name            string
	Directory       string
	Command         []string
	ExitCode        int
	Stdout          string
	Stderr          string
	Duration        time.Duration
	TimedOut        bool
	StdoutTruncated bool
	StderrTruncated bool
}

// DiagnosticDetails formats one bounded check result for a private local
// diagnostic report.
func (result Result) DiagnosticDetails() string {
	var details strings.Builder
	fmt.Fprintf(&details, "name: %q\ndirectory: %q\nargv: %q\nexit_code: %d\nduration: %s\ntimed_out: %t\nstdout_truncated: %t\nstderr_truncated: %t\n", result.Name, result.Directory, result.Command, result.ExitCode, result.Duration, result.TimedOut, result.StdoutTruncated, result.StderrTruncated)
	if result.Stdout != "" {
		fmt.Fprintf(&details, "stdout:\n%s\n", result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprintf(&details, "stderr:\n%s\n", result.Stderr)
	}
	return strings.TrimSpace(details.String())
}

// Passed reports whether the check started, completed, and exited successfully.
func (result Result) Passed() bool {
	return !result.TimedOut && result.ExitCode == 0
}

// Runner is the deterministic-check seam used by workflow orchestration.
type Runner interface {
	Run(context.Context, string, []Definition) ([]Result, error)
}

// Executor runs checks using the shared direct child-process primitive.
type Executor struct {
	process processrun.Runner
}

// NewExecutor constructs a deterministic check executor.
func NewExecutor(process processrun.Runner) *Executor {
	return &Executor{process: process}
}

// Run executes every configured check in order. Nonzero exits are returned as
// repairable evidence; inability to start or finish a check is a technical
// failure and stops the set.
func (executor *Executor) Run(ctx context.Context, worktree string, definitions []Definition) ([]Result, error) {
	if executor == nil || executor.process == nil {
		return nil, errors.New("check executor is not configured")
	}
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree {
		return nil, errors.New("check worktree must be an absolute clean path")
	}

	results := make([]Result, 0, len(definitions))
	for index, definition := range definitions {
		if err := validateDefinition(index, definition); err != nil {
			return results, err
		}
		checkDirectory, err := resolveDirectory(worktree, definition)
		if err != nil {
			return results, err
		}
		checkContext, cancel := context.WithTimeout(ctx, definition.Timeout)
		processResult, err := executor.process.Run(checkContext, processrun.Request{
			Directory:        checkDirectory,
			Argv:             append([]string(nil), definition.Command...),
			Environment:      sanitizedEnvironment(),
			CleanEnvironment: true,
			StdoutLimit:      outputLimit,
			StderrLimit:      outputLimit,
		})
		contextErr := checkContext.Err()
		cancel()

		result := resultFromProcess(definition, processResult)
		results = append(results, result)
		if err == nil {
			continue
		}
		var exitError *processrun.ExitError
		if errors.As(err, &exitError) {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(contextErr, context.DeadlineExceeded) {
			results[len(results)-1].TimedOut = true
			return results, fmt.Errorf("check %q timed out after %s: %w", definition.Name, definition.Timeout, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(contextErr, context.Canceled) {
			return results, fmt.Errorf("check %q was canceled: %w", definition.Name, err)
		}
		return results, fmt.Errorf("start check %q: %w", definition.Name, err)
	}
	return results, nil
}

func validateDefinition(index int, definition Definition) error {
	if strings.TrimSpace(definition.Name) == "" {
		return fmt.Errorf("check %d name must not be empty", index)
	}
	if len(definition.Command) == 0 || strings.TrimSpace(definition.Command[0]) == "" {
		return fmt.Errorf("check %q command must contain an executable", definition.Name)
	}
	if !ValidDirectory(definition.Directory) {
		return fmt.Errorf("check %q directory must be a repository-relative directory", definition.Name)
	}
	if definition.Timeout <= 0 {
		return fmt.Errorf("check %q timeout must be positive", definition.Name)
	}
	return nil
}

// ValidDirectory reports whether directory is empty (the worktree root) or a
// normalized repository-relative directory.
func ValidDirectory(directory string) bool {
	if directory == "" {
		return true
	}
	if strings.ContainsRune(directory, '\x00') || strings.Contains(directory, `\`) || filepath.IsAbs(directory) {
		return false
	}
	cleaned := filepath.Clean(filepath.FromSlash(directory))
	return cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) && filepath.ToSlash(cleaned) == directory
}

func resolveDirectory(worktree string, definition Definition) (string, error) {
	if definition.Directory == "" {
		return worktree, nil
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", fmt.Errorf("resolve check worktree: %w", err)
	}
	target := filepath.Join(worktree, filepath.FromSlash(definition.Directory))
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve check %q directory %q: %w", definition.Name, definition.Directory, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect check %q directory %q: %w", definition.Name, definition.Directory, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("check %q directory %q is not a directory", definition.Name, definition.Directory)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("check %q directory must stay inside the worktree", definition.Name)
	}
	return resolved, nil
}

func resultFromProcess(definition Definition, result processrun.Result) Result {
	return Result{
		Name:            definition.Name,
		Directory:       definition.Directory,
		Command:         append([]string(nil), definition.Command...),
		ExitCode:        result.ExitCode,
		Stdout:          string(result.Stdout),
		Stderr:          string(result.Stderr),
		Duration:        result.Duration,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
	}
}

func sanitizedEnvironment() map[string]string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found || credentialVariable(name) {
			continue
		}
		environment[name] = value
	}
	environment["AWDEV_WORKER"] = "1"
	return environment
}

func credentialVariable(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "CODEX_")
}
