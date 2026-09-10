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
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestReconRunsReadOnlyThroughAgentAbstractionAndPersistsArtifact(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events}
	renderer := &specificationPrompt{events: &events, rendered: "recon prompt"}
	runner := &specificationAgent{events: &events, result: agent.RunResult{
		FinalOutput: []byte(`{"recon":"# Recon\n\n## Issue\n\nAdd recon.\n"}`),
		SessionID:   "recon-session",
	}}
	decoder := &reconDecoder{events: &events, outcome: agent.ReconOutcome{Recon: "# Recon\n\n## Issue\n\nAdd recon.\n"}}
	artifacts := &reconArtifacts{events: &events}
	schemaPath := filepath.Join(t.TempDir(), "recon-result.schema.json")
	service := workflow.NewReconnaissanceService(stateStore, stateStore, artifacts, renderer, runner, decoder, schemaPath, time.Minute)

	result, err := service.Recon(context.Background(), controllerRoot, manifest.WorkflowID)
	if err != nil {
		t.Fatalf("recon: %v", err)
	}
	wantEvents := []string{"read", "transition:spec/running", "render", "agent", "decode:recon", "save:recon"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.Manifest.Phase != state.PhaseSpec || result.Manifest.Substep != state.SubstepRecon || result.Manifest.Status != state.StatusRunning {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
	if string(artifacts.contents) != decoder.outcome.Recon || result.Recon != decoder.outcome.Recon {
		t.Fatalf("persisted recon = %q, result = %q", artifacts.contents, result.Recon)
	}
	if runner.request.Access != agent.AccessReadOnly || runner.request.Worktree != specificationWorktree(t, controllerRoot, manifest) || runner.request.OutputSchema != schemaPath {
		t.Fatalf("agent request = %#v", runner.request)
	}
	if renderer.data.WorktreePath != runner.request.Worktree || renderer.data.ReconPath != ".awdev/issues/"+manifest.WorkflowID+"/recon.md" || renderer.data.Issue.Title != manifest.Issue.Title {
		t.Fatalf("prompt data = %#v", renderer.data)
	}
}

func TestReconFailurePreventsArtifactAndPersistsSpecReconFailure(t *testing.T) {
	manifest, controllerRoot := specificationManifest(t)
	events := []string{}
	stateStore := &specificationState{manifest: manifest, events: &events}
	artifacts := &reconArtifacts{events: &events}
	service := workflow.NewReconnaissanceService(
		stateStore,
		stateStore,
		artifacts,
		&specificationPrompt{events: &events, rendered: "recon prompt"},
		&specificationAgent{events: &events, err: errors.New("agent failed")},
		&reconDecoder{events: &events},
		filepath.Join(t.TempDir(), "recon-result.schema.json"),
		time.Minute,
	)

	result, err := service.Recon(context.Background(), controllerRoot, manifest.WorkflowID)
	if err == nil || !strings.Contains(err.Error(), "run recon agent") {
		t.Fatalf("error = %v, want recon agent failure", err)
	}
	if artifacts.called {
		t.Fatal("recon artifact was saved after agent failure")
	}
	if result.Manifest.Phase != state.PhaseSpec || result.Manifest.Substep != state.SubstepRecon || result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil {
		t.Fatalf("failed manifest = %#v", result.Manifest)
	}
}

type reconArtifacts struct {
	events   *[]string
	called   bool
	contents []byte
	err      error
}

func (store *reconArtifacts) SaveRecon(_ string, _ string, contents []byte) error {
	*store.events = append(*store.events, "save:recon")
	store.called = true
	store.contents = append([]byte(nil), contents...)
	return store.err
}

type reconDecoder struct {
	events  *[]string
	outcome agent.ReconOutcome
	err     error
}

func (decoder *reconDecoder) Decode(_ []byte) (agent.ReconOutcome, error) {
	*decoder.events = append(*decoder.events, "decode:recon")
	return decoder.outcome, decoder.err
}

var _ workflow.SpecificationPromptRenderer = (*specificationPrompt)(nil)
var _ promptRendererCompatibility = (*specificationPrompt)(nil)

type promptRendererCompatibility interface {
	Render(prompt.PromptData) (string, error)
}
