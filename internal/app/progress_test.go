package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

func TestProgressAgentRunnerReportsStartAndHeartbeatUntilCompletion(t *testing.T) {
	inner := &waitingAgentRunner{started: make(chan struct{}), finish: make(chan struct{})}
	messages := make(chan string, 16)
	runner := &progressAgentRunner{
		runner:   inner,
		report:   func(message string) { messages <- message },
		interval: 10 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), agent.Request{})
		done <- err
	}()

	wantMessage(t, messages, "Creating specification with Codex. This can take a few minutes...")
	select {
	case <-inner.started:
	case <-time.After(time.Second):
		t.Fatal("inner agent did not start")
	}
	wantMessage(t, messages, "Codex is still working...")
	close(inner.finish)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	for {
		select {
		case <-messages:
			continue
		default:
			goto drained
		}
	}

drained:
	select {
	case message := <-messages:
		t.Fatalf("progress continued after completion: %q", message)
	case <-time.After(30 * time.Millisecond):
	}
}

func wantMessage(t *testing.T, messages <-chan string, want string) {
	t.Helper()
	select {
	case got := <-messages:
		if got != want {
			t.Fatalf("progress message = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("progress message %q was not reported", want)
	}
}

type waitingAgentRunner struct {
	started chan struct{}
	finish  chan struct{}
	once    sync.Once
}

func (runner *waitingAgentRunner) Run(context.Context, agent.Request) (agent.RunResult, error) {
	runner.once.Do(func() { close(runner.started) })
	<-runner.finish
	return agent.RunResult{}, nil
}
