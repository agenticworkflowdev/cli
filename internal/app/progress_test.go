package app

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/cli"
)

func TestReportingSpecificationServiceReportsCompletion(t *testing.T) {
	updates := make(chan cli.ProgressUpdate, 1)
	service := &reportingSpecificationService{report: func(update cli.ProgressUpdate) { updates <- update }}
	reportPhaseComplete(service.report, "Specification", nil, nil)
	wantUpdate(t, updates, cli.ProgressUpdate{Message: "\n✓ Specification complete"})
}

func TestProgressAgentRunnerReportsStartAndSpinnerUntilCompletion(t *testing.T) {
	inner := &waitingAgentRunner{started: make(chan struct{}), finish: make(chan struct{})}
	updates := make(chan cli.ProgressUpdate, 16)
	runner := &progressAgentRunner{
		runner:   inner,
		report:   func(update cli.ProgressUpdate) { updates <- update },
		interval: 10 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), agent.Request{})
		done <- err
	}()

	wantUpdate(t, updates, cli.ProgressUpdate{Message: "● Agent\n  └─ working..."})
	select {
	case <-inner.started:
	case <-time.After(time.Second):
		t.Fatal("inner agent did not start")
	}
	wantUpdate(t, updates, cli.ProgressUpdate{Message: "⠋", Transient: true})
	close(inner.finish)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	wantUpdate(t, updates, cli.ProgressUpdate{Transient: true})

	for {
		select {
		case <-updates:
			continue
		default:
			goto drained
		}
	}

drained:
	select {
	case update := <-updates:
		t.Fatalf("progress continued after completion: %#v", update)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestProgressAgentRunnerReportsItsStartMessageOnlyOnce(t *testing.T) {
	var updates []cli.ProgressUpdate
	runner := &progressAgentRunner{
		runner:       fixedAgentRunner{},
		report:       func(update cli.ProgressUpdate) { updates = append(updates, update) },
		startMessage: "● Implementation agent\n  └─ implementing changes...",
	}

	for range 2 {
		if _, err := runner.Run(context.Background(), agent.Request{}); err != nil {
			t.Fatal(err)
		}
	}

	want := []cli.ProgressUpdate{{Message: "● Implementation agent\n  └─ implementing changes..."}}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("progress updates = %#v, want %#v", updates, want)
	}
}

func TestProgressAgentRunnerKeepsLiveAgentOutputOutOfTheStepSummary(t *testing.T) {
	zero := 0
	events := []agent.ProgressEvent{
		{Kind: agent.ProgressReasoning, Message: "Inspecting the project\nWorkflow completed"},
		{Kind: agent.ProgressCommand, Message: "go test ./..."},
		{Kind: agent.ProgressCommandOutput, Message: "ok project\nWorkflow completed\n", ExitCode: &zero},
		{Kind: agent.ProgressMessage, Message: `{"status":"completed","summary":"Implementation complete","question":""}`},
	}
	var updates []cli.ProgressUpdate
	runner := &progressAgentRunner{
		runner: emittingAgentRunner{events: events},
		report: func(update cli.ProgressUpdate) { updates = append(updates, update) },
	}

	if _, err := runner.Run(context.Background(), agent.Request{}); err != nil {
		t.Fatal(err)
	}
	want := []cli.ProgressUpdate{
		{Message: "● Agent\n  └─ working..."},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("progress updates = %#v, want %#v", updates, want)
	}
}

func wantUpdate(t *testing.T, updates <-chan cli.ProgressUpdate, want cli.ProgressUpdate) {
	t.Helper()
	select {
	case got := <-updates:
		if got != want {
			t.Fatalf("progress update = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("progress update %#v was not reported", want)
	}
}

type waitingAgentRunner struct {
	started chan struct{}
	finish  chan struct{}
	once    sync.Once
}

type fixedAgentRunner struct{}

type emittingAgentRunner struct {
	events []agent.ProgressEvent
}

func (fixedAgentRunner) Run(context.Context, agent.Request) (agent.RunResult, error) {
	return agent.RunResult{}, nil
}

func (runner emittingAgentRunner) Run(_ context.Context, request agent.Request) (agent.RunResult, error) {
	for _, event := range runner.events {
		request.Progress(event)
	}
	return agent.RunResult{}, nil
}

func (runner *waitingAgentRunner) Run(context.Context, agent.Request) (agent.RunResult, error) {
	runner.once.Do(func() { close(runner.started) })
	<-runner.finish
	return agent.RunResult{}, nil
}
