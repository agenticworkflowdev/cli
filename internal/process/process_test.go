//go:build unix

package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

func TestRunnerPreservesInvocationAndBoundsOutput(t *testing.T) {
	t.Setenv("AWDEV_TEST_VALUE", "base value")
	directory := t.TempDir()
	runner := processrun.NewRunner()
	result, err := runner.Run(context.Background(), processrun.Request{
		Directory: directory,
		Argv:      helperArgv("contract", "literal;$(touch nope)", "line\ntwo"),
		Stdin:     []byte("stdin body"),
		Environment: map[string]string{
			"AWDEV_TEST_VALUE": "added value",
		},
		StdoutLimit: 512,
		StderrLimit: 4,
	})
	if err != nil {
		t.Fatalf("run helper: %v", err)
	}
	wantParts := []string{directory, "literal;$(touch nope)", "line\ntwo", "stdin body", "added value"}
	for _, want := range wantParts {
		if !strings.Contains(string(result.Stdout), want) {
			t.Errorf("stdout %q does not contain %q", result.Stdout, want)
		}
	}
	if string(result.Stderr) != "0123" || !result.StderrTruncated {
		t.Fatalf("stderr = %q, truncated = %v; want bounded output", result.Stderr, result.StderrTruncated)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestRunnerReturnsExitMetadata(t *testing.T) {
	result, err := processrun.NewRunner().Run(context.Background(), processrun.Request{Argv: helperArgv("exit", "23")})
	var exitError *processrun.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("error = %v, want ExitError", err)
	}
	if result.ExitCode != 23 || exitError.Result.ExitCode != 23 || string(result.Stderr) != "failed" {
		t.Fatalf("result = %#v, exit error = %#v", result, exitError)
	}
	details := exitError.DiagnosticDetails()
	for _, want := range []string{"exit_code: 23", "stderr:\nfailed", "-test.run=TestProcessHelper"} {
		if !strings.Contains(details, want) {
			t.Errorf("diagnostic details do not contain %q:\n%s", want, details)
		}
	}
}

func TestRunnerBoundsBothOutputStreams(t *testing.T) {
	result, err := processrun.NewRunner().Run(context.Background(), processrun.Request{
		Argv:        helperArgv("output"),
		StdoutLimit: 5,
		StderrLimit: 4,
	})
	if err != nil {
		t.Fatalf("run helper: %v", err)
	}
	if string(result.Stdout) != "abcde" || !result.StdoutTruncated {
		t.Fatalf("stdout = %q, truncated = %v", result.Stdout, result.StdoutTruncated)
	}
	if string(result.Stderr) != "0123" || !result.StderrTruncated {
		t.Fatalf("stderr = %q, truncated = %v", result.Stderr, result.StderrTruncated)
	}
}

func TestRunnerCanReplaceTheInheritedEnvironment(t *testing.T) {
	t.Setenv("AWDEV_UNRELATED_SECRET", "must-not-leak")
	result, err := processrun.NewRunner().Run(context.Background(), processrun.Request{
		Argv:             helperArgv("environment"),
		CleanEnvironment: true,
		Environment:      map[string]string{"AWDEV_WORKER": "1"},
	})
	if err != nil {
		t.Fatalf("run helper: %v", err)
	}
	if got, want := string(result.Stdout), "AWDEV_WORKER=1"; got != want {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}

func TestRunnerCancellationTerminatesProcessGroup(t *testing.T) {
	directory := t.TempDir()
	childReady := filepath.Join(directory, "child-ready")
	grandchildReady := filepath.Join(directory, "grandchild-ready")
	childSurvived := filepath.Join(directory, "child-survived")
	grandchildSurvived := filepath.Join(directory, "grandchild-survived")
	ctx, cancel := context.WithCancel(context.Background())
	runner := processrun.NewRunner()
	runner.KillGrace = 50 * time.Millisecond
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, processrun.Request{Argv: helperArgv("tree", childReady, grandchildReady, childSurvived, grandchildSurvived)})
		done <- err
	}()
	waitForFiles(t, childReady, grandchildReady)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation took too long: %s", time.Since(started))
	}
	time.Sleep(500 * time.Millisecond)
	for _, marker := range []string{childSurvived, grandchildSurvived} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("process descendant survived cancellation and wrote %s: %v", marker, err)
		}
	}
}

func TestRunnerCancellationPreservesPartialDiagnostics(t *testing.T) {
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := processrun.NewRunner().Run(ctx, processrun.Request{Argv: helperArgv("cancel-output", ready)})
		done <- err
	}()
	waitForFiles(t, ready)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	var diagnostic interface{ DiagnosticDetails() string }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error = %T %v, want diagnostic details", err, err)
	}
	details := diagnostic.DiagnosticDetails()
	for _, want := range []string{"stdout:\npartial stdout", "stderr:\npartial stderr", "termination_timed_out: false"} {
		if !strings.Contains(details, want) {
			t.Errorf("diagnostic details do not contain %q:\n%s", want, details)
		}
	}
}

func waitForFiles(t *testing.T, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allExist := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				allExist = false
				break
			}
		}
		if allExist {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process tree did not become ready: %v", paths)
}

func helperArgv(arguments ...string) []string {
	return append([]string{os.Args[0], "-test.run=TestProcessHelper", "--"}, arguments...)
}

func TestProcessHelper(t *testing.T) {
	separator := 0
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index + 1
			break
		}
	}
	if separator == 0 || separator >= len(os.Args) {
		return
	}
	arguments := os.Args[separator:]
	switch arguments[0] {
	case "contract":
		input, _ := os.ReadFile("/dev/stdin")
		directory, _ := os.Getwd()
		fmt.Fprintf(os.Stdout, "%s|%s|%s|%s|%s", directory, arguments[1], arguments[2], input, os.Getenv("AWDEV_TEST_VALUE"))
		fmt.Fprint(os.Stderr, "0123456789")
	case "exit":
		fmt.Fprint(os.Stderr, "failed")
		code, _ := strconv.Atoi(arguments[1])
		os.Exit(code)
	case "output":
		fmt.Fprint(os.Stdout, "abcdefghijklmnopqrstuvwxyz")
		fmt.Fprint(os.Stderr, "0123456789")
	case "environment":
		fmt.Fprint(os.Stdout, strings.Join(os.Environ(), "\n"))
	case "cancel-output":
		fmt.Fprint(os.Stdout, "partial stdout")
		fmt.Fprint(os.Stderr, "partial stderr")
		_ = os.WriteFile(arguments[1], []byte("ready"), 0o600)
		time.Sleep(24 * time.Hour)
	case "tree":
		command := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "tree-child", arguments[1], arguments[2], arguments[3], arguments[4])
		if err := command.Start(); err != nil {
			os.Exit(2)
		}
		_ = command.Wait()
	case "tree-child":
		signal.Ignore(syscall.SIGTERM)
		_ = os.WriteFile(arguments[1], []byte("ready"), 0o600)
		command := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "tree-grandchild", arguments[2], arguments[4])
		if err := command.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(arguments[3], []byte("survived"), 0o600)
		_ = command.Wait()
	case "tree-grandchild":
		signal.Ignore(syscall.SIGTERM)
		_ = os.WriteFile(arguments[1], []byte("ready"), 0o600)
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(arguments[2], []byte("survived"), 0o600)
		select {}
	}
	os.Exit(0)
}
