package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestReconRunsReadOnlyThroughAgentAbstractionAndPersistsArtifact(t *testing.T) {
	manifest, controllerRoot := reconManifest(t)
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
	wantEvents := []string{"read", "transition:recon/running", "render", "agent", "decode:recon", "save:recon", "read:recon"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.Manifest.Phase != state.PhaseRecon || result.Manifest.Status != state.StatusRunning {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
	if string(artifacts.contents) != decoder.outcome.Recon || result.Recon != decoder.outcome.Recon {
		t.Fatalf("persisted recon = %q, result = %q", artifacts.contents, result.Recon)
	}
	if runner.request.Access != agent.AccessReadOnly || runner.request.Worktree != specificationWorktree(t, controllerRoot, manifest) || runner.request.OutputSchema != schemaPath {
		t.Fatalf("agent request = %#v", runner.request)
	}
	wantPromptData := prompt.PromptData{
		WorkflowID:   manifest.WorkflowID,
		Repository:   manifest.Repository,
		Branch:       manifest.Branch,
		BaseSHA:      manifest.BaseSHA,
		WorktreePath: runner.request.Worktree,
		ReconPath:    ".awdev/issues/" + manifest.WorkflowID + "/recon.md",
		Issue: prompt.IssueData{
			Number: manifest.Issue.Number,
			Title:  manifest.Issue.Title,
			Body:   manifest.Issue.Body,
			URL:    manifest.Issue.URL,
		},
	}
	if !reflect.DeepEqual(renderer.data, wantPromptData) {
		t.Fatalf("prompt data = %#v", renderer.data)
	}
}

func TestReconCodeIntelligencePreferenceAndFallbackThroughAgentBoundary(t *testing.T) {
	reconPromptContents, err := fs.ReadFile(assets.Defaults(), "prompts/recon.md")
	if err != nil {
		t.Fatal(err)
	}
	reconPrompt, err := prompt.NewRenderer("recon", reconPromptContents)
	if err != nil {
		t.Fatal(err)
	}
	schemaContents, err := fs.ReadFile(assets.Defaults(), "schemas/recon-result.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := agent.NewReconResultDecoder(schemaContents)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		source         *fakeReconCodeIntelligence
		wantDiscovery  string
		wantSourceCall bool
	}{
		{
			name: "available configured source is preferred",
			source: &fakeReconCodeIntelligence{
				result: "repository-provided semantic index",
			},
			wantDiscovery:  "repository-provided semantic index",
			wantSourceCall: true,
		},
		{
			name:          "no configured source falls back silently",
			wantDiscovery: "targeted git inventory and search",
		},
		{
			name: "optional source failure falls back",
			source: &fakeReconCodeIntelligence{
				err: errors.New("optional index unavailable"),
			},
			wantDiscovery:  "targeted git inventory and search",
			wantSourceCall: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, controllerRoot := reconManifest(t)
			events := []string{}
			stateStore := &specificationState{manifest: manifest, events: &events}
			artifacts := &reconArtifacts{events: &events}
			runner := &reconDiscoveryAgent{events: &events, source: test.source}
			service := workflow.NewReconnaissanceService(
				stateStore,
				stateStore,
				artifacts,
				reconPrompt,
				runner,
				decoder,
				filepath.Join(t.TempDir(), "recon-result.schema.json"),
				time.Minute,
			)

			result, err := service.Recon(context.Background(), controllerRoot, manifest.WorkflowID)
			if err != nil {
				t.Fatalf("recon: %v", err)
			}
			if !strings.Contains(result.Recon, test.wantDiscovery) {
				t.Fatalf("recon = %q, want discovery via %q", result.Recon, test.wantDiscovery)
			}
			if test.source != nil && test.source.called != test.wantSourceCall {
				t.Fatalf("configured source called = %t, want %t", test.source.called, test.wantSourceCall)
			}
			if runner.request.Access != agent.AccessReadOnly {
				t.Fatalf("agent access = %q, want read-only", runner.request.Access)
			}
		})
	}
}

func TestReconFailurePreventsArtifactAndPersistsSpecReconFailure(t *testing.T) {
	manifest, controllerRoot := reconManifest(t)
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
	if result.Manifest.Phase != state.PhaseRecon || result.Manifest.Status != state.StatusFailed || result.Manifest.LastError == nil {
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

func (store *reconArtifacts) ReadRecon(_ string, _ string) ([]byte, error) {
	*store.events = append(*store.events, "read:recon")
	return append([]byte(nil), store.contents...), store.err
}

type reconDecoder struct {
	events  *[]string
	outcome agent.ReconOutcome
	err     error
}

type fakeReconCodeIntelligence struct {
	called bool
	result string
	err    error
}

func (source *fakeReconCodeIntelligence) Inspect() (string, error) {
	source.called = true
	return source.result, source.err
}

// reconDiscoveryAgent models the provider-neutral agent boundary: configured
// intelligence is attempted first and normal targeted discovery remains a
// successful path when that optional facility is absent or fails.
type reconDiscoveryAgent struct {
	events  *[]string
	source  *fakeReconCodeIntelligence
	request agent.Request
}

func (runner *reconDiscoveryAgent) Run(_ context.Context, request agent.Request) (agent.RunResult, error) {
	*runner.events = append(*runner.events, "agent")
	runner.request = request
	if !strings.Contains(request.Prompt, "discover and prefer code-intelligence facilities already configured") ||
		!strings.Contains(request.Prompt, "If no suitable facility exists, silently continue") ||
		!strings.Contains(request.Prompt, "If an optional facility fails, fall back") {
		return agent.RunResult{}, errors.New("recon prompt does not define code-intelligence preference and fallback")
	}

	discovery := ""
	if runner.source != nil {
		discovery, _ = runner.source.Inspect()
	}
	if discovery == "" {
		discovery = "targeted git inventory and search"
	}
	contents, err := json.Marshal(agent.ReconOutcome{Recon: "# Recon\n\n## Existing patterns\n\n- Discovery used " + discovery + ".\n"})
	if err != nil {
		return agent.RunResult{}, err
	}
	return agent.RunResult{FinalOutput: contents}, nil
}

var _ agent.Runner = (*reconDiscoveryAgent)(nil)

func (decoder *reconDecoder) Decode(_ []byte) (agent.ReconOutcome, error) {
	*decoder.events = append(*decoder.events, "decode:recon")
	return decoder.outcome, decoder.err
}

var _ workflow.SpecificationPromptRenderer = (*specificationPrompt)(nil)
var _ promptRendererCompatibility = (*specificationPrompt)(nil)

type promptRendererCompatibility interface {
	Render(prompt.PromptData) (string, error)
}

func reconManifest(t *testing.T) (state.Manifest, string) {
	t.Helper()
	manifest, controllerRoot := specificationManifest(t)
	manifest.Phase = state.PhaseInit
	return manifest, controllerRoot
}
