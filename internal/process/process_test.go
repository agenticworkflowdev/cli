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

func TestRunnerCancellationTerminatesProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-finished")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runner := processrun.NewRunner()
	runner.KillGrace = 50 * time.Millisecond
	started := time.Now()
	_, err := runner.Run(ctx, processrun.Request{Argv: helperArgv("tree", marker)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation took too long: %s", time.Since(started))
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived cancellation and wrote marker: %v", err)
	}
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
	case "tree":
		if os.Getenv("AWDEV_DESCENDANT") == "1" {
			signal.Ignore(syscall.SIGTERM)
			for {
				time.Sleep(400 * time.Millisecond)
				_ = os.WriteFile(arguments[1], []byte("survived"), 0o644)
			}
		}
		command := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", "tree", arguments[1])
		command.Env = append(os.Environ(), "AWDEV_DESCENDANT=1")
		if err := command.Start(); err != nil {
			os.Exit(2)
		}
		_ = syscall.Kill(command.Process.Pid, syscall.SIGCONT)
		_ = command.Wait()
	}
	os.Exit(0)
}
