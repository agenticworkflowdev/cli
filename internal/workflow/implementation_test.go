package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/checks"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestImplementationPersistsRunningBeforeAgentAndReturnsPassingEvidence(t *testing.T) {
	events := []string{}
	manifest, controllerRoot := implementationManifest(t)
	stateStore := &implementationState{manifest: manifest, events: &events}
	implementPrompt := &implementationPrompt{label: "implement", rendered: "implementation prompt", events: &events}
	repairPrompt := &implementationPrompt{label: "repair", rendered: "repair prompt", events: &events}
	agentRunner := &implementationAgent{events: &events, responses: []agentResponse{{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`), SessionID: "thread-1"}}}}
	decoder := &implementationDecoder{events: &events, outcomes: []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "done"}}}
	checkResults := []checks.Result{{Name: "tests", Command: []string{"go", "test", "./..."}, ExitCode: 0, Duration: time.Second}}
	checkRunner := &implementationChecks{events: &events, results: [][]checks.Result{checkResults}}
	diff := &implementationDiff{events: &events}
	definitions := []checks.Definition{{Name: "tests", Command: []string{"go", "test", "./..."}, Timeout: time.Minute}}
	service := workflow.NewImplementationService(stateStore, stateStore, implementPrompt, repairPrompt, agentRunner, decoder, checkRunner, diff, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), time.Minute, definitions, []string{"generated"})

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("implement: %v", err)
	}
	wantEvents := []string{"read", "transition:implementation/running", "render:implement", "diff:capture", "agent", "diff:inspect", "decode", "worktree:capture", "checks", "worktree:inspect"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusRunning || !reflect.DeepEqual(result.CheckResults, checkResults) {
		t.Fatalf("result = %#v", result)
	}
	if result.Blocker != nil || !reflect.DeepEqual(result.SessionIDs, []string{"thread-1"}) {
		t.Fatalf("result metadata = %#v", result)
	}
	if implementPrompt.data.SpecificationPath != manifest.SpecificationPath || len(implementPrompt.data.CheckResults) != 0 {
		t.Fatalf("implementation prompt data = %#v", implementPrompt.data)
	}
	worktree := implementationWorktree(t, controllerRoot, manifest)
	request := agentRunner.requests[0]
	if request.Prompt != "implementation prompt" || request.Worktree != worktree || request.Access != agent.AccessWorkspaceWrite {
		t.Fatalf("agent request = %#v", request)
	}
	if _, ok := agentRunner.contexts[0].Deadline(); !ok {
		t.Fatal("implementation agent did not receive its own timeout")
	}
	if checkRunner.worktrees[0] != worktree || !reflect.DeepEqual(checkRunner.definitions[0], definitions) {
		t.Fatalf("check call = worktree %q definitions %#v", checkRunner.worktrees[0], checkRunner.definitions[0])
	}
	if !reflect.DeepEqual(diff.protected[0], []string{"generated"}) {
		t.Fatalf("protected paths = %#v", diff.protected)
	}
}

func TestImplementationDoesNotTrustChecksThatMutateTheWorktree(t *testing.T) {
	events := []string{}
	manifest, controllerRoot := implementationManifest(t)
	stateStore := &implementationState{manifest: manifest, events: &events}
	worktree := &worktreeStateFake{events: &events, changed: [][]string{{"generated.go"}}}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		&implementationAgent{events: &events, responses: []agentResponse{{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`)}}}},
		&implementationDecoder{events: &events, outcomes: []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "done"}}},
		&implementationChecks{events: &events, results: [][]checks.Result{{{Name: "tests", ExitCode: 0}}}},
		&implementationDiff{events: &events}, worktree,
		filepath.Join(t.TempDir(), "schema.json"), time.Minute, []checks.Definition{{Name: "tests", Command: []string{"go", "test"}, Timeout: time.Minute}}, nil,
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err == nil || !strings.Contains(err.Error(), "deterministic checks changed worktree paths") {
		t.Fatalf("error = %v", err)
	}
	if result.Manifest.Status != state.StatusFailed || !reflect.DeepEqual(result.CheckedState, gitrepo.WorktreeBaseline{}) {
		t.Fatalf("mutating checks produced trusted evidence: %#v", result)
	}
}

func TestImplementationRepairsFailedChecksWithExactEvidenceAndRerunsAllChecks(t *testing.T) {
	events := []string{}
	manifest, controllerRoot := implementationManifest(t)
	stateStore := &implementationState{manifest: manifest, events: &events}
	implementPrompt := &implementationPrompt{label: "implement", rendered: "implement", events: &events}
	repairPrompt := &implementationPrompt{label: "repair", rendered: "repair", events: &events}
	agentRunner := &implementationAgent{events: &events, responses: []agentResponse{
		{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"initial"}`), SessionID: "initial"}},
		{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"fixed"}`), SessionID: "repair-1"}},
	}}
	decoder := &implementationDecoder{events: &events, outcomes: []agent.Outcome{
		{Status: agent.OutcomeCompleted, Summary: "initial"},
		{Status: agent.OutcomeCompleted, Summary: "fixed"},
	}}
	failed := checks.Result{Name: "unit", Command: []string{"go", "test", "./..."}, ExitCode: 1, Stdout: "exact stdout", Stderr: "exact stderr", Duration: 2 * time.Second, StderrTruncated: true}
	passingOther := checks.Result{Name: "lint", Command: []string{"go", "vet", "./..."}, ExitCode: 0}
	passing := []checks.Result{{Name: "unit", Command: failed.Command, ExitCode: 0}, passingOther}
	checkRunner := &implementationChecks{events: &events, results: [][]checks.Result{{failed, passingOther}, passing}}
	diff := &implementationDiff{events: &events}
	definitions := []checks.Definition{
		{Name: "unit", Command: failed.Command, Timeout: time.Minute},
		{Name: "lint", Command: passingOther.Command, Timeout: time.Minute},
	}
	service := workflow.NewImplementationService(stateStore, stateStore, implementPrompt, repairPrompt, agentRunner, decoder, checkRunner, diff, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), time.Minute, definitions, nil)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("implement: %v", err)
	}
	if len(agentRunner.requests) != 2 || agentRunner.requests[1].Prompt != "repair" || len(checkRunner.definitions) != 2 {
		t.Fatalf("agent calls = %d, check calls = %d", len(agentRunner.requests), len(checkRunner.definitions))
	}
	if diff.inspects != 2 {
		t.Fatalf("post-agent scope inspections = %d, want one per workspace-write run", diff.inspects)
	}
	if !reflect.DeepEqual(repairPrompt.data.CheckResults, []checks.Result{failed}) {
		t.Fatalf("repair evidence = %#v, want exact failed result %#v", repairPrompt.data.CheckResults, failed)
	}
	for index := range checkRunner.definitions {
		if !reflect.DeepEqual(checkRunner.definitions[index], definitions) {
			t.Errorf("check run %d definitions = %#v", index, checkRunner.definitions[index])
		}
	}
	if !reflect.DeepEqual(result.CheckResults, passing) || !reflect.DeepEqual(result.SessionIDs, []string{"initial", "repair-1"}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestImplementationStopsAfterThreeCheckFixInvocations(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events}
	agentRunner := &implementationAgent{events: &events}
	decoder := &implementationDecoder{events: &events}
	for index := 0; index < 4; index++ {
		agentRunner.responses = append(agentRunner.responses, agentResponse{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"still failing"}`)}})
		decoder.outcomes = append(decoder.outcomes, agent.Outcome{Status: agent.OutcomeCompleted, Summary: "still failing"})
	}
	failed := []checks.Result{{Name: "tests", Command: []string{"go", "test", "./..."}, ExitCode: 1, Stderr: "failure"}}
	checkRunner := &implementationChecks{events: &events, results: [][]checks.Result{failed, failed, failed, failed}}
	repairPrompt := &implementationPrompt{label: "repair", rendered: "repair", events: &events}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		repairPrompt,
		agentRunner, decoder, checkRunner, &implementationDiff{events: &events}, &worktreeStateFake{events: &events},
		filepath.Join(t.TempDir(), "schema.json"), time.Minute,
		[]checks.Definition{{Name: "tests", Command: []string{"go", "test", "./..."}, Timeout: time.Minute}}, nil,
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err == nil || !strings.Contains(err.Error(), "failed after 3 repair attempts") {
		t.Fatalf("error = %v, want repair exhaustion", err)
	}
	if len(agentRunner.requests) != 4 || len(checkRunner.definitions) != 4 {
		t.Fatalf("agent calls = %d, check calls = %d; want initial plus three repairs", len(agentRunner.requests), len(checkRunner.definitions))
	}
	if len(repairPrompt.datas) != 3 {
		t.Fatalf("repair prompt renders = %d, want 3", len(repairPrompt.datas))
	}
	for index, data := range repairPrompt.datas {
		if !reflect.DeepEqual(data.CheckResults, failed) {
			t.Errorf("repair %d evidence = %#v, want %#v", index+1, data.CheckResults, failed)
		}
	}
	if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil || !strings.Contains(result.Manifest.LastError.Message, "3 repair attempts") {
		t.Fatalf("failed manifest = %#v", result.Manifest)
	}
	var diagnostic interface{ DiagnosticDetails() string }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error %T does not retain diagnostic details", err)
	}
	for _, want := range []string{`name: "tests"`, `argv: ["go" "test" "./..."]`, "stderr:\nfailure"} {
		if !strings.Contains(diagnostic.DiagnosticDetails(), want) {
			t.Errorf("diagnostic details do not contain %q:\n%s", want, diagnostic.DiagnosticDetails())
		}
	}
}

func TestImplementationForwardsBlockerWithoutRunningChecks(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events}
	checkRunner := &implementationChecks{events: &events}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		&implementationAgent{events: &events, responses: []agentResponse{{result: agent.RunResult{FinalOutput: []byte(`{"status":"blocked","question":"Which API?"}`)}}}},
		&implementationDecoder{events: &events, outcomes: []agent.Outcome{{Status: agent.OutcomeBlocked, Question: "Which API?"}}},
		checkRunner, &implementationDiff{events: &events}, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), time.Minute, nil, nil,
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Blocker == nil || result.Blocker.Phase != state.PhaseImplementation || result.Blocker.Question != "Which API?" {
		t.Fatalf("blocker = %#v", result.Blocker)
	}
	if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusRunning || result.Manifest.LastError != nil || len(checkRunner.definitions) != 0 {
		t.Fatalf("blocked result = %#v; check calls = %d", result.Manifest, len(checkRunner.definitions))
	}
}

func TestImplementationRejectsProtectedDiffWithoutRepair(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events}
	agentRunner := &implementationAgent{events: &events, responses: []agentResponse{{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`)}}}}
	checkRunner := &implementationChecks{events: &events}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		agentRunner, &implementationDecoder{events: &events}, checkRunner,
		&implementationDiff{events: &events, offending: [][]string{{".gitmodules", "generated/secret.txt"}}},
		&worktreeStateFake{events: &events},
		filepath.Join(t.TempDir(), "schema.json"), time.Minute, nil, []string{"generated"},
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if err == nil || !strings.Contains(err.Error(), "protected paths") {
		t.Fatalf("error = %v, want scope violation", err)
	}
	if len(agentRunner.requests) != 1 || len(checkRunner.definitions) != 0 {
		t.Fatalf("scope violation was repaired or checked: agents=%d checks=%d", len(agentRunner.requests), len(checkRunner.definitions))
	}
	if result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil || !strings.Contains(result.Manifest.LastError.Message, ".gitmodules, generated/secret.txt") {
		t.Fatalf("failed result = %#v", result.Manifest)
	}
}

func TestImplementationTechnicalFailuresPersistSanitizedFailedState(t *testing.T) {
	tests := []struct {
		name       string
		agentError error
		decodeErr  error
		checkError error
		want       string
	}{
		{name: "agent", agentError: errors.New("secret output\nsecond line"), want: "run implementation agent"},
		{name: "malformed result", decodeErr: errors.New("malformed result"), want: "validate implementation agent result"},
		{name: "check timeout", checkError: context.DeadlineExceeded, want: "run deterministic checks"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, controllerRoot := implementationManifest(t)
			events := []string{}
			stateStore := &implementationState{manifest: manifest, events: &events}
			service := workflow.NewImplementationService(
				stateStore, stateStore,
				&implementationPrompt{label: "implement", rendered: "implement", events: &events},
				&implementationPrompt{label: "repair", rendered: "repair", events: &events},
				&implementationAgent{events: &events, responses: []agentResponse{{result: agent.RunResult{FinalOutput: []byte(`{"status":"completed","summary":"done"}`)}, err: test.agentError}}},
				&implementationDecoder{events: &events, outcomes: []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "done"}}, errors: []error{test.decodeErr}},
				&implementationChecks{events: &events, errors: []error{test.checkError}},
				&implementationDiff{events: &events}, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), time.Minute,
				[]checks.Definition{{Name: "tests", Command: []string{"go", "test"}, Timeout: time.Minute}}, nil,
			)

			result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil || strings.Contains(result.Manifest.LastError.Message, "\n") || strings.Contains(result.Manifest.LastError.Message, controllerRoot) {
				t.Fatalf("failed manifest = %#v", result.Manifest)
			}
		})
	}
}

func TestImplementationAppliesConfiguredTimeoutToAgentInvocation(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events}
	runner := &blockingImplementationAgent{events: &events}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		runner, &implementationDecoder{events: &events}, &implementationChecks{events: &events},
		&implementationDiff{events: &events}, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), 10*time.Millisecond, nil, nil,
	)

	result, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if runner.calls != 1 || result.Manifest.Status != state.StatusFailed {
		t.Fatalf("calls = %d, result = %#v", runner.calls, result)
	}
}

func TestImplementationDoesNotStartAgentBeforeRunningTransitionIsDurable(t *testing.T) {
	manifest, controllerRoot := implementationManifest(t)
	events := []string{}
	stateStore := &implementationState{manifest: manifest, events: &events, transitionErr: errors.New("disk full")}
	runner := &implementationAgent{events: &events}
	service := workflow.NewImplementationService(
		stateStore, stateStore,
		&implementationPrompt{label: "implement", rendered: "implement", events: &events},
		&implementationPrompt{label: "repair", rendered: "repair", events: &events},
		runner, &implementationDecoder{events: &events}, &implementationChecks{events: &events},
		&implementationDiff{events: &events}, &worktreeStateFake{events: &events}, filepath.Join(t.TempDir(), "schema.json"), time.Minute, nil, nil,
	)

	if _, err := service.Implement(context.Background(), controllerRoot, manifest.WorkflowID); err == nil {
		t.Fatal("transition failure was ignored")
	}
	if len(runner.requests) != 0 {
		t.Fatal("agent started before implementation/running was durable")
	}
}

type implementationState struct {
	manifest      state.Manifest
	events        *[]string
	transitionErr error
}

func (store *implementationState) Read(_ string, _ string) (state.Manifest, error) {
	*store.events = append(*store.events, "read")
	return store.manifest, nil
}

func (store *implementationState) Transition(_ string, _ string, next state.Manifest) error {
	*store.events = append(*store.events, "transition:"+string(next.Phase)+"/"+string(next.Status))
	if store.transitionErr != nil {
		return store.transitionErr
	}
	store.manifest = next
	return nil
}

type implementationPrompt struct {
	label    string
	rendered string
	events   *[]string
	data     prompt.PromptData
	datas    []prompt.PromptData
	err      error
}

func (renderer *implementationPrompt) Render(data prompt.PromptData) (string, error) {
	*renderer.events = append(*renderer.events, "render:"+renderer.label)
	renderer.data = data
	renderer.datas = append(renderer.datas, data)
	return renderer.rendered, renderer.err
}

type agentResponse struct {
	result agent.RunResult
	err    error
}

type implementationAgent struct {
	events    *[]string
	requests  []agent.Request
	responses []agentResponse
	contexts  []context.Context
}

type blockingImplementationAgent struct {
	events *[]string
	calls  int
}

func (runner *blockingImplementationAgent) Run(ctx context.Context, _ agent.Request) (agent.RunResult, error) {
	*runner.events = append(*runner.events, "agent")
	runner.calls++
	<-ctx.Done()
	return agent.RunResult{}, ctx.Err()
}

func (runner *implementationAgent) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	*runner.events = append(*runner.events, "agent")
	runner.requests = append(runner.requests, request)
	runner.contexts = append(runner.contexts, ctx)
	response := runner.responses[len(runner.requests)-1]
	return response.result, response.err
}

type implementationDecoder struct {
	events   *[]string
	outcomes []agent.Outcome
	errors   []error
	calls    int
}

func (decoder *implementationDecoder) Decode(_ []byte) (agent.Outcome, error) {
	*decoder.events = append(*decoder.events, "decode")
	index := decoder.calls
	decoder.calls++
	var err error
	if index < len(decoder.errors) {
		err = decoder.errors[index]
	}
	var outcome agent.Outcome
	if index < len(decoder.outcomes) {
		outcome = decoder.outcomes[index]
	}
	return outcome, err
}

type implementationChecks struct {
	events      *[]string
	worktrees   []string
	definitions [][]checks.Definition
	results     [][]checks.Result
	errors      []error
}

func (runner *implementationChecks) Run(_ context.Context, worktree string, definitions []checks.Definition) ([]checks.Result, error) {
	*runner.events = append(*runner.events, "checks")
	runner.worktrees = append(runner.worktrees, worktree)
	runner.definitions = append(runner.definitions, append([]checks.Definition(nil), definitions...))
	index := len(runner.definitions) - 1
	var result []checks.Result
	if index < len(runner.results) {
		result = append([]checks.Result(nil), runner.results[index]...)
	}
	var err error
	if index < len(runner.errors) {
		err = runner.errors[index]
	}
	return result, err
}

type implementationDiff struct {
	events    *[]string
	offending [][]string
	errors    []error
	protected [][]string
	inspects  int
}

func (inspector *implementationDiff) Capture(_ gitrepo.DiffScope) (gitrepo.DiffBaseline, error) {
	*inspector.events = append(*inspector.events, "diff:capture")
	return gitrepo.DiffBaseline{}, nil
}

func (inspector *implementationDiff) Inspect(_ context.Context, scope gitrepo.DiffScope, _ gitrepo.DiffBaseline) ([]string, error) {
	*inspector.events = append(*inspector.events, "diff:inspect")
	index := inspector.inspects
	inspector.inspects++
	inspector.protected = append(inspector.protected, append([]string(nil), scope.ProtectedPrefixes...))
	var offending []string
	if index < len(inspector.offending) {
		offending = append([]string(nil), inspector.offending[index]...)
	}
	var err error
	if index < len(inspector.errors) {
		err = inspector.errors[index]
	}
	return offending, err
}

func implementationManifest(t *testing.T) (state.Manifest, string) {
	t.Helper()
	manifest, controllerRoot := specificationManifest(t)
	manifest.Phase = state.PhaseSpec
	manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
	return manifest, controllerRoot
}

func implementationWorktree(t *testing.T, controllerRoot string, manifest state.Manifest) string {
	t.Helper()
	worktree, err := state.ResolveWorktreePath(controllerRoot, manifest.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	return worktree
}
