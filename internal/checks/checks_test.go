package checks_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/checks"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestExecutorRunsEveryCheckInDeclaredOrderWithSanitizedEnvironment(t *testing.T) {
	t.Setenv("PATH", "/test/bin")
	t.Setenv("HOME", "/test/home")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("CODEX_ACCESS_TOKEN", "codex-secret")
	t.Setenv("CODEX_CUSTOM_SECRET", "another-secret")
	process := &recordingProcessRunner{responses: []processResponse{
		{result: processrun.Result{ExitCode: 7, Stdout: []byte("failed stdout"), Stderr: []byte("failed stderr"), Duration: 2 * time.Second, StdoutTruncated: true}, err: &processrun.ExitError{Argv: []string{"go"}, Result: processrun.Result{ExitCode: 7}}},
		{result: processrun.Result{ExitCode: 0, Stdout: []byte("passed"), Duration: time.Second}},
	}}
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, "services", "api"), 0o755); err != nil {
		t.Fatalf("create nested check directory: %v", err)
	}
	executor := checks.NewExecutor(process)
	definitions := []checks.Definition{
		{Name: "unit", Directory: "services/api", Command: []string{"go", "test", "./..."}, Timeout: time.Minute},
		{Name: "lint", Command: []string{"go", "vet", "./..."}, Timeout: 2 * time.Minute},
	}

	results, err := executor.Run(context.Background(), worktree, definitions)
	if err != nil {
		t.Fatalf("run checks: %v", err)
	}
	if len(results) != 2 || results[0].Passed() || !results[1].Passed() {
		t.Fatalf("results = %#v", results)
	}
	if got := results[0]; got.Name != "unit" || !reflect.DeepEqual(got.Command, definitions[0].Command) || got.ExitCode != 7 || got.Stdout != "failed stdout" || got.Stderr != "failed stderr" || got.Duration != 2*time.Second || !got.StdoutTruncated || got.TimedOut {
		t.Fatalf("first result = %#v", got)
	}
	if len(process.requests) != 2 {
		t.Fatalf("process calls = %d, want 2", len(process.requests))
	}
	for index, request := range process.requests {
		wantDirectory := worktree
		if definitions[index].Directory != "" {
			resolvedWorktree, err := filepath.EvalSymlinks(worktree)
			if err != nil {
				t.Fatalf("resolve expected worktree: %v", err)
			}
			wantDirectory = filepath.Join(resolvedWorktree, filepath.FromSlash(definitions[index].Directory))
		}
		if request.Directory != wantDirectory || !reflect.DeepEqual(request.Argv, definitions[index].Command) {
			t.Errorf("request %d = %#v", index, request)
		}
		if !request.CleanEnvironment || request.StdoutLimit <= 0 || request.StderrLimit <= 0 || !request.PreserveOutputTail {
			t.Errorf("request %d did not use a bounded clean environment: %#v", index, request)
		}
		if request.Environment["PATH"] != "/test/bin" || request.Environment["HOME"] != "/test/home" || request.Environment["AWDEV_WORKER"] != "1" {
			t.Errorf("request %d environment lost safe variables: %#v", index, request.Environment)
		}
		for name := range request.Environment {
			upper := strings.ToUpper(name)
			if strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "CODEX_") {
				t.Errorf("request %d leaked credential variable %q", index, name)
			}
		}
	}
}

func TestExecutorRejectsNestedCheckDirectoryThatEscapesThroughSymlink(t *testing.T) {
	worktree := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(worktree, "linked-app")); err != nil {
		t.Fatalf("create escaping symlink: %v", err)
	}
	process := &recordingProcessRunner{}

	_, err := checks.NewExecutor(process).Run(context.Background(), worktree, []checks.Definition{{
		Name: "build", Directory: "linked-app", Command: []string{"npm", "run", "build"}, Timeout: time.Minute,
	}})

	if err == nil || !strings.Contains(err.Error(), `check "build" directory must stay inside the worktree`) {
		t.Fatalf("error = %v, want worktree escape rejection", err)
	}
	if len(process.requests) != 0 {
		t.Fatalf("process calls = %d, want none", len(process.requests))
	}
}

func TestExecutorRunsGoCheckFromConfiguredNestedModule(t *testing.T) {
	worktree := t.TempDir()
	module := filepath.Join(worktree, "nested-module")
	if err := os.Mkdir(module, 0o755); err != nil {
		t.Fatalf("create nested module: %v", err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.test/nested\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write nested go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(module, "nested.go"), []byte("package nested\n"), 0o644); err != nil {
		t.Fatalf("write nested package: %v", err)
	}

	results, err := checks.NewExecutor(processrun.NewRunner()).Run(context.Background(), worktree, []checks.Definition{{
		Name: "tests", Directory: "nested-module", Command: []string{"go", "test", "./..."}, Timeout: time.Minute,
	}})

	if err != nil {
		t.Fatalf("run nested module check: %v", err)
	}
	if len(results) != 1 || !results[0].Passed() {
		t.Fatalf("nested module results = %#v, want one passing check", results)
	}
}

func TestExecutorReturnsTimeoutAsTechnicalFailureWithTypedMetadata(t *testing.T) {
	processResult := processrun.Result{ExitCode: -1, Stdout: []byte("partial"), Duration: 3 * time.Second}
	process := &recordingProcessRunner{responses: []processResponse{{
		result: processResult,
		err: &processrun.ContextError{
			Argv: []string{"slow-check"}, Result: processResult, Cause: context.DeadlineExceeded, PID: 42,
		},
	}}}
	executor := checks.NewExecutor(process)

	results, err := executor.Run(context.Background(), "/repo/worktree", []checks.Definition{{Name: "slow", Command: []string{"slow-check"}, Timeout: time.Second}})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if len(results) != 1 || !results[0].TimedOut || results[0].Stdout != "partial" || results[0].Duration != 3*time.Second {
		t.Fatalf("timeout results = %#v", results)
	}
	if process.contexts[0].Deadline().IsZero() {
		t.Fatal("check process did not receive its own deadline")
	}
}

func TestExecutorTreatsStartErrorsAsTechnicalFailures(t *testing.T) {
	process := &recordingProcessRunner{responses: []processResponse{{result: processrun.Result{ExitCode: -1}, err: errors.New("executable missing")}}}
	results, err := checks.NewExecutor(process).Run(context.Background(), "/repo/worktree", []checks.Definition{{Name: "missing", Command: []string{"missing"}, Timeout: time.Minute}})
	if err == nil || !strings.Contains(err.Error(), `start check "missing"`) {
		t.Fatalf("error = %v, want start failure", err)
	}
	if len(results) != 1 || results[0].ExitCode != -1 {
		t.Fatalf("results = %#v", results)
	}
}

type processResponse struct {
	result processrun.Result
	err    error
}

type recordingProcessRunner struct {
	requests  []processrun.Request
	contexts  []recordedContext
	responses []processResponse
}

type recordedContext struct {
	deadline time.Time
	ok       bool
}

func (context recordedContext) Deadline() time.Time {
	return context.deadline
}

func (runner *recordingProcessRunner) Run(ctx context.Context, request processrun.Request) (processrun.Result, error) {
	deadline, ok := ctx.Deadline()
	runner.contexts = append(runner.contexts, recordedContext{deadline: deadline, ok: ok})
	runner.requests = append(runner.requests, request)
	response := runner.responses[len(runner.requests)-1]
	return response.result, response.err
}
