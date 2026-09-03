package workflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestSpecificationPhasePersistsTransitionBeforeRenderingAndRunning(t *testing.T) {
	events := []string{}
	manifest, controllerRoot := specificationManifest(t)
	stateStore := &specificationState{manifest: manifest, events: &events}
	renderer := &specificationPrompt{events: &events, rendered: "rendered prompt"}
	runner := &specificationAgent{events: &events, run: func(request agent.Request) (agent.RunResult, error) {
		specificationPath := filepath.Join(request.Worktree, ".awdev", "specs", "gh-17-a-title.md")
		if err := os.MkdirAll(filepath.Dir(specificationPath), 0o755); err != nil {
			return agent.RunResult{}, err
		}
		if err := os.WriteFile(specificationPath, []byte("# Specification\n"), 0o644); err != nil {
			return agent.RunResult{}, err
		}
		return agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`), SessionID: "thread-1"}, nil
	}}
	decoder := &specificationDecoder{events: &events, outcome: agent.Outcome{Status: agent.OutcomeCompleted, Summary: "done"}}
	service := workflow.NewSpecificationService(stateStore, stateStore, renderer, runner, decoder, filepath.Join(t.TempDir(), "agent-result.schema.json"), time.Minute)

	result, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("create specification: %v", err)
	}
	wantEvents := []string{"read", "transition:spec/running", "render", "agent", "decode", "transition:spec/running"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.Manifest.SpecificationPath != ".awdev/specs/gh-17-a-title.md" || result.Manifest.Status != state.StatusRunning {
		t.Fatalf("result manifest = %#v", result.Manifest)
	}
	if renderer.data.Issue.Body != manifest.Issue.Body || renderer.data.Issue.Title != manifest.Issue.Title {
		t.Fatalf("prompt data did not use persisted issue snapshot: %#v", renderer.data)
	}
	if renderer.data.SpecificationPath != result.Manifest.SpecificationPath {
		t.Fatalf("prompt specification path = %q, want %q", renderer.data.SpecificationPath, result.Manifest.SpecificationPath)
	}
	if runner.request.Prompt != "rendered prompt" || runner.request.Access != agent.AccessWorkspaceWrite || runner.request.Worktree != specificationWorktree(t, controllerRoot, manifest) {
		t.Fatalf("agent request = %#v", runner.request)
	}
	manifestPath, err := state.ManifestPath(controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if runner.request.OutputDirectory != filepath.Dir(manifestPath) {
		t.Fatalf("output directory = %q, want %q", runner.request.OutputDirectory, filepath.Dir(manifestPath))
	}
}

func TestSpecificationPhaseReturnsTypedBlockerWithoutMisclassifyingItAsFailure(t *testing.T) {
	events := []string{}
	manifest, controllerRoot := specificationManifest(t)
	stateStore := &specificationState{manifest: manifest, events: &events}
	service := workflow.NewSpecificationService(
		stateStore,
		stateStore,
		&specificationPrompt{events: &events, rendered: "prompt"},
		&specificationAgent{events: &events, result: agent.RunResult{FinalOutput: []byte(`{"status":"blocked","question":"Which API?"}`)}},
		&specificationDecoder{events: &events, outcome: agent.Outcome{Status: agent.OutcomeBlocked, Question: "Which API?"}},
		filepath.Join(t.TempDir(), "schema.json"),
		time.Minute,
	)

	result, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("create specification: %v", err)
	}
	if result.Blocker == nil || result.Blocker.Phase != state.PhaseSpec || result.Blocker.Question != "Which API?" {
		t.Fatalf("blocker = %#v", result.Blocker)
	}
	if result.Manifest.Phase != state.PhaseSpec || result.Manifest.Status != state.StatusRunning || result.Manifest.LastError != nil {
		t.Fatalf("blocked result mutated technical state: %#v", result.Manifest)
	}
	wantEvents := []string{"read", "transition:spec/running", "render", "agent", "decode"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestSpecificationPhaseRejectsUnsafeOrMissingSpecificationPostconditions(t *testing.T) {
	setups := map[string]func(*testing.T, string, state.Manifest){
		"missing": func(*testing.T, string, state.Manifest) {},
		"empty": func(t *testing.T, controllerRoot string, manifest state.Manifest) {
			writeSpecification(t, controllerRoot, manifest, nil)
		},
		"directory": func(t *testing.T, controllerRoot string, manifest state.Manifest) {
			if err := os.MkdirAll(specificationAbsolutePath(t, controllerRoot, manifest), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"file symlink escape": func(t *testing.T, controllerRoot string, manifest state.Manifest) {
			outside := filepath.Join(t.TempDir(), "outside.md")
			if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(specificationAbsolutePath(t, controllerRoot, manifest)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, specificationAbsolutePath(t, controllerRoot, manifest)); err != nil {
				t.Fatal(err)
			}
		},
		"parent symlink escape": func(t *testing.T, controllerRoot string, manifest state.Manifest) {
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, manifest.WorkflowID+".md"), []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			worktree := specificationWorktree(t, controllerRoot, manifest)
			if err := os.MkdirAll(filepath.Join(worktree, ".awdev"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(worktree, ".awdev", "specs")); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			manifest, controllerRoot := specificationManifest(t)
			setup(t, controllerRoot, manifest)
			events := []string{}
			stateStore := &specificationState{manifest: manifest, events: &events}
			service := workflow.NewSpecificationService(
				stateStore,
				stateStore,
				&specificationPrompt{events: &events, rendered: "prompt"},
				&specificationAgent{events: &events, result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`)}},
				&specificationDecoder{events: &events, outcome: agent.Outcome{Status: agent.OutcomeCompleted, Summary: "done"}},
				filepath.Join(t.TempDir(), "schema.json"),
				time.Minute,
			)
			if _, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID); err == nil {
				t.Fatal("unsafe specification postcondition was accepted")
			}
			if stateStore.manifest.Phase != state.PhaseSpec || stateStore.manifest.Status != state.StatusFailed || stateStore.manifest.LastError == nil {
				t.Fatalf("failure state = %#v", stateStore.manifest)
			}
			if stateStore.manifest.SpecificationPath != "" {
				t.Fatalf("failed state recorded specification path %q", stateStore.manifest.SpecificationPath)
			}
		})
	}
}

func TestSpecificationPhasePersistsTechnicalFailuresWithSanitizedErrors(t *testing.T) {
	tests := []struct {
		name       string
		promptErr  error
		runnerErr  error
		decoderErr error
	}{
		{name: "prompt", promptErr: errors.New("bad\nprompt")},
		{name: "runner", runnerErr: errors.New("secret output\nsecond line")},
		{name: "result", decoderErr: errors.New("invalid schema result")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, controllerRoot := specificationManifest(t)
			events := []string{}
			stateStore := &specificationState{manifest: manifest, events: &events}
			service := workflow.NewSpecificationService(
				stateStore,
				stateStore,
				&specificationPrompt{events: &events, rendered: "prompt", err: test.promptErr},
				&specificationAgent{events: &events, err: test.runnerErr},
				&specificationDecoder{events: &events, err: test.decoderErr},
				filepath.Join(t.TempDir(), "schema.json"),
				time.Minute,
			)
			if _, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID); err == nil {
				t.Fatal("technical failure was ignored")
			}
			failure := stateStore.manifest.LastError
			if stateStore.manifest.Status != state.StatusFailed || failure == nil || failure.Code != "technical_failure" {
				t.Fatalf("failure state = %#v", stateStore.manifest)
			}
			if strings.ContainsAny(failure.Message, "\r\n") || len(failure.Message) > 500 {
				t.Fatalf("error was not sanitized: %q", failure.Message)
			}
		})
	}
}

func TestSpecificationPhasePersistsControllerPathsAsRelativeErrors(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events}
	absoluteSchemaPath := filepath.Join(controllerRoot, ".awdev", "schemas", "agent-result.schema.json")
	service := workflow.NewSpecificationService(
		stateStore,
		stateStore,
		&specificationPrompt{events: &events, rendered: "prompt"},
		&specificationAgent{events: &events, err: errors.New("cannot read " + absoluteSchemaPath)},
		&specificationDecoder{events: &events},
		absoluteSchemaPath,
		time.Minute,
	)

	if _, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID); err == nil {
		t.Fatal("technical failure was ignored")
	}
	failure := stateStore.manifest.LastError
	if failure == nil {
		t.Fatal("technical failure was not persisted")
	}
	if strings.Contains(failure.Message, controllerRoot) {
		t.Fatalf("persisted error contains controller root: %q", failure.Message)
	}
	if !strings.Contains(failure.Message, ".awdev/schemas/agent-result.schema.json") {
		t.Fatalf("persisted error = %q, want relative controller path", failure.Message)
	}
}

func TestSpecificationPhaseAppliesConfiguredTimeout(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events}
	runner := &specificationAgent{events: &events, runContext: func(ctx context.Context, _ agent.Request) (agent.RunResult, error) {
		<-ctx.Done()
		return agent.RunResult{}, ctx.Err()
	}}
	service := workflow.NewSpecificationService(
		stateStore,
		stateStore,
		&specificationPrompt{events: &events, rendered: "prompt"},
		runner,
		&specificationDecoder{events: &events},
		filepath.Join(t.TempDir(), "schema.json"),
		10*time.Millisecond,
	)
	if _, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if stateStore.manifest.Status != state.StatusFailed {
		t.Fatalf("timeout state = %#v", stateStore.manifest)
	}
}

func TestSpecificationPhaseHonorsCallerCancellation(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events}
	runnerStarted := make(chan struct{})
	runner := &specificationAgent{events: &events, runContext: func(ctx context.Context, _ agent.Request) (agent.RunResult, error) {
		close(runnerStarted)
		<-ctx.Done()
		return agent.RunResult{}, ctx.Err()
	}}
	service := workflow.NewSpecificationService(
		stateStore,
		stateStore,
		&specificationPrompt{events: &events, rendered: "prompt"},
		runner,
		&specificationDecoder{events: &events},
		filepath.Join(t.TempDir(), "schema.json"),
		time.Minute,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := service.Create(ctx, controllerRoot, manifest.WorkflowID)
		done <- err
	}()
	<-runnerStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if stateStore.manifest.Status != state.StatusFailed {
		t.Fatalf("cancellation state = %#v", stateStore.manifest)
	}
}

func TestSpecificationPhaseDoesNotStartAgentWhenRunningTransitionFails(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events, transitionErr: errors.New("disk full")}
	runner := &specificationAgent{events: &events}
	service := workflow.NewSpecificationService(
		stateStore,
		stateStore,
		&specificationPrompt{events: &events, rendered: "prompt"},
		runner,
		&specificationDecoder{events: &events},
		filepath.Join(t.TempDir(), "schema.json"),
		time.Minute,
	)
	if _, err := service.Create(context.Background(), controllerRoot, manifest.WorkflowID); err == nil {
		t.Fatal("transition failure was ignored")
	}
	if runner.called {
		t.Fatal("agent started before spec/running was durable")
	}
}

type specificationState struct {
	manifest      state.Manifest
	events        *[]string
	transitionErr error
}

func (store *specificationState) Read(_ string, _ string) (state.Manifest, error) {
	*store.events = append(*store.events, "read")
	return store.manifest, nil
}

func (store *specificationState) Transition(_ string, _ string, next state.Manifest) error {
	*store.events = append(*store.events, "transition:"+string(next.Phase)+"/"+string(next.Status))
	if store.transitionErr != nil {
		return store.transitionErr
	}
	store.manifest = next
	return nil
}

type specificationPrompt struct {
	events   *[]string
	data     prompt.PromptData
	rendered string
	err      error
}

func (renderer *specificationPrompt) Render(data prompt.PromptData) (string, error) {
	*renderer.events = append(*renderer.events, "render")
	renderer.data = data
	return renderer.rendered, renderer.err
}

type specificationAgent struct {
	events     *[]string
	called     bool
	request    agent.Request
	result     agent.RunResult
	err        error
	run        func(agent.Request) (agent.RunResult, error)
	runContext func(context.Context, agent.Request) (agent.RunResult, error)
}

func (runner *specificationAgent) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	*runner.events = append(*runner.events, "agent")
	runner.called = true
	runner.request = request
	if runner.runContext != nil {
		return runner.runContext(ctx, request)
	}
	if runner.run != nil {
		return runner.run(request)
	}
	return runner.result, runner.err
}

type specificationDecoder struct {
	events  *[]string
	outcome agent.Outcome
	err     error
}

func (decoder *specificationDecoder) Decode(_ []byte) (agent.Outcome, error) {
	*decoder.events = append(*decoder.events, "decode")
	return decoder.outcome, decoder.err
}

func specificationManifest(t *testing.T) (state.Manifest, string) {
	t.Helper()
	root := t.TempDir()
	relativeWorktree := ".awdev/worktrees/gh-17-a-title"
	worktree := filepath.Join(root, filepath.FromSlash(relativeWorktree))
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	return state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    fixedWorkflowID,
		Source:        state.SourceGitHub,
		Repository:    "owner/repository",
		DefaultBranch: "main",
		Issue: state.IssueSnapshot{
			Number: 17, Title: "A title {{.WorkflowID}}", Body: "line one\nUnicode — <!-- marker --> {{printf \"unsafe\"}}", URL: "https://github.com/owner/repository/issues/17", State: "OPEN", UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		},
		Actor:    "octocat",
		Phase:    state.PhaseInit,
		Status:   state.StatusRunning,
		Branch:   "gh-17-a-title",
		BaseSHA:  strings.Repeat("a", 40),
		Worktree: relativeWorktree,
	}, root
}

func specificationWorktree(t *testing.T, controllerRoot string, manifest state.Manifest) string {
	t.Helper()
	worktree, err := state.ResolveWorktreePath(controllerRoot, manifest.Worktree)
	if err != nil {
		t.Fatalf("resolve specification worktree: %v", err)
	}
	return worktree
}

func specificationAbsolutePath(t *testing.T, controllerRoot string, manifest state.Manifest) string {
	t.Helper()
	return filepath.Join(specificationWorktree(t, controllerRoot, manifest), ".awdev", "specs", manifest.Branch+".md")
}

func writeSpecification(t *testing.T, controllerRoot string, manifest state.Manifest, contents []byte) {
	t.Helper()
	path := specificationAbsolutePath(t, controllerRoot, manifest)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}
