package assets_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	"github.com/agenticworkflowdev/cli/internal/review"
)

func TestDefaultAgentResultSchemaUsesCodexSupportedRootObject(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "schemas/agent-result.schema.json")
	if err != nil {
		t.Fatalf("read default agent result schema: %v", err)
	}

	var schema struct {
		Type       string                     `json:"type"`
		OneOf      json.RawMessage            `json:"oneOf"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(contents, &schema); err != nil {
		t.Fatalf("decode default agent result schema: %v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("root type = %q, want object", schema.Type)
	}
	if len(schema.OneOf) != 0 {
		t.Fatal("root oneOf is not supported by Codex structured output")
	}
	for property := range schema.Properties {
		if !slices.Contains(schema.Required, property) {
			t.Errorf("property %q is not required", property)
		}
	}

	decoder, err := agent.NewResultDecoder(contents)
	if err != nil {
		t.Fatalf("compile default agent result schema: %v", err)
	}
	for _, result := range []string{
		`{"status":"completed","summary":"Specification written","question":""}`,
		`{"status":"blocked","summary":"","question":"Which API should be stable?"}`,
	} {
		if _, err := decoder.Decode([]byte(result)); err != nil {
			t.Errorf("decode %s: %v", result, err)
		}
	}
}

func TestDefaultReconResultSchemaProducesValidatedMarkdown(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "schemas/recon-result.schema.json")
	if err != nil {
		t.Fatalf("read default recon result schema: %v", err)
	}
	decoder, err := agent.NewReconResultDecoder(contents)
	if err != nil {
		t.Fatalf("compile default recon result schema: %v", err)
	}
	if _, err := decoder.Decode([]byte(`{"recon":"# Recon\n\n## Issue\n\nAdd recon.\n"}`)); err != nil {
		t.Fatalf("decode default recon result: %v", err)
	}
}

func TestDefaultReviewResultSchemaRequiresEveryFindingPropertyForCodex(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "schemas/review-result.schema.json")
	if err != nil {
		t.Fatalf("read default review result schema: %v", err)
	}

	var schema struct {
		Properties struct {
			Findings struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                   `json:"required"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(contents, &schema); err != nil {
		t.Fatalf("decode default review result schema: %v", err)
	}
	for property := range schema.Properties.Findings.Items.Properties {
		if !slices.Contains(schema.Properties.Findings.Items.Required, property) {
			t.Errorf("finding property %q is not required", property)
		}
	}

	decoder, err := review.NewResultDecoder(contents)
	if err != nil {
		t.Fatalf("compile default review result schema: %v", err)
	}
	result := []byte(`{"approved":false,"findings":[{"severity":"medium","path":null,"line":null,"message":"Explain the failure."}]}`)
	if _, err := decoder.Decode(result); err != nil {
		t.Errorf("decode finding without a source location: %v", err)
	}
}

func TestLoadInstalledUsesControllerRootAndRepositoryOwnedBytes(t *testing.T) {
	root := t.TempDir()
	if _, err := initrepo.Initialize(root, agent.ProviderCodex, initrepo.Options{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	wantPrompt := []byte("repository-owned spec prompt\n")
	promptPath := filepath.Join(root, ".awdev", "prompts", "spec.md")
	if err := os.WriteFile(promptPath, wantPrompt, 0o644); err != nil {
		t.Fatalf("edit prompt: %v", err)
	}

	installed, err := assets.LoadInstalled(root)
	if err != nil {
		t.Fatalf("load installed assets: %v", err)
	}
	if len(installed.Prompts) != 7 {
		t.Fatalf("prompt count = %d, want 7", len(installed.Prompts))
	}
	if len(installed.Schemas) != 3 {
		t.Fatalf("schema count = %d, want 3", len(installed.Schemas))
	}
	if got := installed.Prompts[assets.PromptSpec]; string(got.Contents) != string(wantPrompt) || got.Path != promptPath {
		t.Fatalf("spec prompt = %#v, want path %q and repository bytes %q", got, promptPath, wantPrompt)
	}
	wantReconPromptPath := filepath.Join(root, ".awdev", "prompts", "recon.md")
	if got := installed.Prompts[assets.PromptRecon]; got.Path != wantReconPromptPath || len(got.Contents) == 0 {
		t.Fatalf("recon prompt = %#v, want non-empty file at %q", got, wantReconPromptPath)
	}
	wantSchemaPath := filepath.Join(root, ".awdev", "schemas", "agent-result.schema.json")
	if got := installed.Schemas[assets.SchemaAgentResult]; got.Path != wantSchemaPath || len(got.Contents) == 0 {
		t.Fatalf("agent schema = %#v, want non-empty file at %q", got, wantSchemaPath)
	}
	if installed.Config.Path != filepath.Join(root, ".awdev", "config.json") || len(installed.Config.Contents) == 0 {
		t.Fatalf("config asset = %#v, want repository config", installed.Config)
	}
}
