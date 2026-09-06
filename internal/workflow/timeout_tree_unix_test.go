//go:build unix

package workflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/checks"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

const ignoringProcessTreeScript = `
trap '' TERM
: > "$1"
(
  trap '' TERM
  : > "$2"
  sleep 0.5
  : > "$4"
) &
sleep 0.5
: > "$3"
wait
`

func TestConfiguredCheckTimeoutKillsIgnoringProcessGroup(t *testing.T) {
	directory := t.TempDir()
	markers := processTreeMarkers(directory)
	processRunner := processrun.NewRunner()
	processRunner.KillGrace = 20 * time.Millisecond
	executor := checks.NewExecutor(processRunner)

	results, err := executor.Run(context.Background(), directory, []checks.Definition{{
		Name: "hung check", Command: processTreeCommand(markers), Timeout: 200 * time.Millisecond,
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if len(results) != 1 || !results[0].TimedOut {
		t.Fatalf("timeout results = %#v", results)
	}
	assertProcessTreeWasKilled(t, markers)
}

func TestConfiguredImplementationAgentTimeoutKillsIgnoringProcessGroup(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events}
	markers := processTreeMarkers(t.TempDir())
	processRunner := processrun.NewRunner()
	processRunner.KillGrace = 20 * time.Millisecond
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		&processTreeAgent{runner: processRunner, command: processTreeCommand(markers), events: &events},
		&implementationDecoder{events: &events}, &implementationChecks{events: &events},
		&implementationDiff{events: &events}, filepath.Join(t.TempDir(), "schema.json"), 200*time.Millisecond, nil, nil,
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if result.Manifest.Status != state.StatusFailed {
		t.Fatalf("timeout result = %#v", result)
	}
	assertProcessTreeWasKilled(t, markers)
}

type treeMarkers struct {
	childReady         string
	grandchildReady    string
	childSurvived      string
	grandchildSurvived string
}

func processTreeMarkers(directory string) treeMarkers {
	return treeMarkers{
		childReady:         filepath.Join(directory, "child-ready"),
		grandchildReady:    filepath.Join(directory, "grandchild-ready"),
		childSurvived:      filepath.Join(directory, "child-survived"),
		grandchildSurvived: filepath.Join(directory, "grandchild-survived"),
	}
}

func processTreeCommand(markers treeMarkers) []string {
	return []string{"sh", "-c", ignoringProcessTreeScript, "sh", markers.childReady, markers.grandchildReady, markers.childSurvived, markers.grandchildSurvived}
}

func assertProcessTreeWasKilled(t *testing.T, markers treeMarkers) {
	t.Helper()
	for _, ready := range []string{markers.childReady, markers.grandchildReady} {
		if _, err := os.Stat(ready); err != nil {
			t.Fatalf("process tree did not start: %s: %v", ready, err)
		}
	}
	time.Sleep(600 * time.Millisecond)
	for _, survived := range []string{markers.childSurvived, markers.grandchildSurvived} {
		if _, err := os.Stat(survived); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("timed-out process survived and wrote %s: %v", survived, err)
		}
	}
}

type processTreeAgent struct {
	runner  processrun.Runner
	command []string
	events  *[]string
}

func (runner *processTreeAgent) Run(ctx context.Context, _ agent.Request) (agent.RunResult, error) {
	*runner.events = append(*runner.events, "agent")
	_, err := runner.runner.Run(ctx, processrun.Request{Argv: runner.command})
	return agent.RunResult{}, err
}
