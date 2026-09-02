// Package process runs child processes without invoking a shell.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
)

// Request describes one direct child-process invocation. Argv contains the
// executable as its first element and is never interpreted by a shell.
type Request struct {
	Directory   string
	Argv        []string
	Stdin       []byte
	Environment map[string]string
	StdoutLimit int
	StderrLimit int
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

// OSRunner directly executes processes and terminates their process group on
// cancellation.
type OSRunner struct {
	KillGrace time.Duration
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

	stdout := newBoundedBuffer(request.StdoutLimit)
	stderr := newBoundedBuffer(request.StderrLimit)
	command := exec.Command(request.Argv[0], request.Argv[1:]...)
	command.Dir = request.Directory
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = mergedEnvironment(request.Environment)
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
		terminateProcessTree(command.Process.Pid, false)
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
		terminateProcessTree(command.Process.Pid, true)
		if !parentExited {
			waitErr = <-waited
		}
		result := processResult(waitErr, stdout, stderr, time.Since(started))
		return result, fmt.Errorf("run %s: %w", request.Argv[0], ctx.Err())
	}

	result := processResult(waitErr, stdout, stderr, time.Since(started))
	if waitErr != nil {
		return result, &ExitError{Argv: append([]string(nil), request.Argv...), Result: result}
	}
	return result, nil
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

func mergedEnvironment(additions map[string]string) []string {
	environment := append([]string(nil), os.Environ()...)
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
	mu        sync.Mutex
	contents  []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(contents []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
