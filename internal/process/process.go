// Package process runs child processes without invoking a shell.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultOutputLimit = 1 << 20
	defaultKillGrace   = 250 * time.Millisecond
	defaultKillWait    = 2 * time.Second
)

// Request describes one direct child-process invocation. Argv contains the
// executable as its first element and is never interpreted by a shell.
type Request struct {
	Directory          string
	Argv               []string
	Stdin              []byte
	Environment        map[string]string
	CleanEnvironment   bool
	StdoutLimit        int
	StderrLimit        int
	PreserveOutputTail bool
	StdoutObserver     func([]byte)
}

// Result records bounded output and process exit metadata.
type Result struct {
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
}

// Runner is the reusable child-process seam.
type Runner interface {
	Run(context.Context, Request) (Result, error)
}

// ExitError reports a child that started but did not exit successfully.
type ExitError struct {
	Argv   []string
	Result Result
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s exited with code %d", e.Argv[0], e.Result.ExitCode)
}

// DiagnosticDetails returns process output and invocation metadata for local
// error logs. It is intentionally not part of Error so terminal failures stay
// concise.
func (e *ExitError) DiagnosticDetails() string {
	if e == nil {
		return ""
	}
	return processDiagnosticDetails(e.Argv, e.Result)
}

// ContextError reports a child terminated because its request context ended,
// while retaining the output captured before termination for diagnostics.
type ContextError struct {
	Argv                   []string
	Result                 Result
	Cause                  error
	PID                    int
	TerminationTimedOut    bool
	ForcedTerminationError error
}

func (e *ContextError) Error() string {
	if e.TerminationTimedOut {
		return fmt.Sprintf("run %s: %v; process %d did not exit after forced termination", e.Argv[0], e.Cause, e.PID)
	}
	return fmt.Sprintf("run %s: %v", e.Argv[0], e.Cause)
}

func (e *ContextError) Unwrap() error {
	return e.Cause
}

// DiagnosticDetails returns the partial process output captured before
// cancellation or timeout.
func (e *ContextError) DiagnosticDetails() string {
	if e == nil {
		return ""
	}
	details := processDiagnosticDetails(e.Argv, e.Result)
	details += fmt.Sprintf("pid: %d\ntermination_timed_out: %t\n", e.PID, e.TerminationTimedOut)
	if e.ForcedTerminationError != nil {
		details += fmt.Sprintf("forced_termination_error: %v\n", e.ForcedTerminationError)
	}
	return details
}

func processDiagnosticDetails(argv []string, result Result) string {
	var details strings.Builder
	fmt.Fprintf(&details, "argv: %q\n", argv)
	fmt.Fprintf(&details, "exit_code: %d\n", result.ExitCode)
	fmt.Fprintf(&details, "duration: %s\n", result.Duration)
	fmt.Fprintf(&details, "stdout_truncated: %t\n", result.StdoutTruncated)
	fmt.Fprintf(&details, "stderr_truncated: %t\n", result.StderrTruncated)
	if len(result.Stdout) > 0 {
		fmt.Fprintf(&details, "stdout:\n%s\n", result.Stdout)
	}
	if len(result.Stderr) > 0 {
		fmt.Fprintf(&details, "stderr:\n%s\n", result.Stderr)
	}
	return details.String()
}

// OSRunner directly executes processes and terminates their process group on
// cancellation.
type OSRunner struct {
	KillGrace time.Duration
	KillWait  time.Duration
}

// NewRunner constructs an operating-system process runner with bounded
// cancellation grace.
func NewRunner() *OSRunner {
	return &OSRunner{KillGrace: defaultKillGrace}
}

// Run executes one request and captures at most the requested number of bytes
// from each output stream while continuing to drain excess output.
func (r *OSRunner) Run(ctx context.Context, request Request) (Result, error) {
	if len(request.Argv) == 0 || strings.TrimSpace(request.Argv[0]) == "" {
		return Result{ExitCode: -1}, errors.New("process argv must contain an executable")
	}
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1}, err
	}

	stdout := newBoundedBuffer(request.StdoutLimit, request.PreserveOutputTail)
	stderr := newBoundedBuffer(request.StderrLimit, request.PreserveOutputTail)
	command := exec.Command(request.Argv[0], request.Argv[1:]...)
	command.Dir = request.Directory
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Stdout = stdout
	if request.StdoutObserver != nil {
		command.Stdout = observedWriter{destination: stdout, observe: request.StdoutObserver}
	}
	command.Stderr = stderr
	command.Env = processEnvironment(request.Environment, request.CleanEnvironment)
	if err := configureProcessTree(command); err != nil {
		return Result{ExitCode: -1}, err
	}

	started := time.Now()
	if err := command.Start(); err != nil {
		return Result{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("start %s: %w", request.Argv[0], err)
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		_ = terminateProcessTree(command.Process.Pid, false)
		grace := r.KillGrace
		if grace <= 0 {
			grace = defaultKillGrace
		}
		timer := time.NewTimer(grace)
		parentExited := false
		select {
		case waitErr = <-waited:
			parentExited = true
			<-timer.C
		case <-timer.C:
		}
		forcedTerminationError := terminateProcessTree(command.Process.Pid, true)
		terminationTimedOut := false
		if !parentExited {
			killWait := r.KillWait
			if killWait <= 0 {
				killWait = defaultKillWait
			}
			killTimer := time.NewTimer(killWait)
			select {
			case waitErr = <-waited:
				killTimer.Stop()
			case <-killTimer.C:
				waitErr = ctx.Err()
				terminationTimedOut = true
			}
		}
		result := processResult(waitErr, stdout, stderr, time.Since(started))
		return result, &ContextError{
			Argv: append([]string(nil), request.Argv...), Result: result, Cause: ctx.Err(), PID: command.Process.Pid,
			TerminationTimedOut: terminationTimedOut, ForcedTerminationError: forcedTerminationError,
		}
	}

	result := processResult(waitErr, stdout, stderr, time.Since(started))
	if waitErr != nil {
		return result, &ExitError{Argv: append([]string(nil), request.Argv...), Result: result}
	}
	return result, nil
}

type observedWriter struct {
	destination io.Writer
	observe     func([]byte)
}

func (writer observedWriter) Write(contents []byte) (int, error) {
	written, err := writer.destination.Write(contents)
	if written > 0 {
		writer.observe(contents[:written])
	}
	return written, err
}

func processResult(waitErr error, stdout, stderr *boundedBuffer, duration time.Duration) Result {
	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return Result{
		ExitCode:        exitCode,
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		Duration:        duration,
	}
}

func processEnvironment(additions map[string]string, clean bool) []string {
	var environment []string
	if !clean {
		environment = append([]string(nil), os.Environ()...)
	}
	keys := make([]string, 0, len(additions))
	for key := range additions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+additions[key])
	}
	return environment
}

type boundedBuffer struct {
	mu           sync.Mutex
	contents     []byte
	limit        int
	preserveTail bool
	truncated    bool
}

func newBoundedBuffer(limit int, preserveTail bool) *boundedBuffer {
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	return &boundedBuffer{limit: limit, preserveTail: preserveTail}
}

func (b *boundedBuffer) Write(contents []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.preserveTail {
		if len(b.contents)+len(contents) > b.limit {
			b.truncated = true
		}
		if len(contents) >= b.limit {
			b.contents = append(b.contents[:0], contents[len(contents)-b.limit:]...)
			return len(contents), nil
		}
		overflow := len(b.contents) + len(contents) - b.limit
		if overflow > 0 {
			copy(b.contents, b.contents[overflow:])
			b.contents = b.contents[:len(b.contents)-overflow]
		}
		b.contents = append(b.contents, contents...)
		return len(contents), nil
	}
	available := b.limit - len(b.contents)
	if available > 0 {
		kept := len(contents)
		if kept > available {
			kept = available
		}
		b.contents = append(b.contents, contents[:kept]...)
	}
	if len(contents) > available {
		b.truncated = true
	}
	return len(contents), nil
}

func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.contents...)
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
