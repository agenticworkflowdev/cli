package agent_test

import (
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

const agentResultSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "oneOf": [
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["status", "summary"],
      "properties": {
        "status": {"const": "completed"},
        "summary": {"type": "string", "minLength": 1}
      }
    },
    {
      "type": "object",
      "additionalProperties": false,
      "required": ["status", "question"],
      "properties": {
        "status": {"const": "blocked"},
        "question": {"type": "string", "minLength": 1}
      }
    }
  ]
}`

func TestResultDecoderValidatesSchemaThenTypedSemantics(t *testing.T) {
	decoder, err := agent.NewResultDecoder([]byte(agentResultSchema))
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	tests := []struct {
		name    string
		input   string
		status  agent.OutcomeStatus
		wantErr string
	}{
		{name: "completed", input: `{"status":"completed","summary":"Specification written"}`, status: agent.OutcomeCompleted},
		{name: "blocked", input: `{"status":"blocked","question":"Which API should be stable?"}`, status: agent.OutcomeBlocked},
		{name: "schema invalid", input: `{"status":"completed"}`, wantErr: "schema"},
		{name: "unknown field", input: `{"status":"completed","summary":"done","extra":true}`, wantErr: "schema"},
		{name: "malformed", input: `{"status":`, wantErr: "decode"},
		{name: "trailing document", input: `{"status":"completed","summary":"done"} {}`, wantErr: "single JSON"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decoder.Decode([]byte(test.input))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Status != test.status {
				t.Fatalf("status = %q, want %q", got.Status, test.status)
			}
		})
	}
}

func TestResultDecoderRejectsSemanticallyImpossibleTypedResults(t *testing.T) {
	decoder, err := agent.NewResultDecoder([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("compile permissive schema: %v", err)
	}
	for _, input := range []string{
		`{"status":"completed","summary":"   "}`,
		`{"status":"blocked","question":"\n"}`,
		`{"status":"other"}`,
		`{"status":"completed","summary":"done","question":"unexpected"}`,
		`{"status":"blocked","summary":"unexpected","question":"why?"}`,
		`{"status":"completed","summary":"done","extra":true}`,
	} {
		if _, err := decoder.Decode([]byte(input)); err == nil {
			t.Fatalf("semantically invalid result was accepted: %s", input)
		}
	}
}

func TestResultDecoderRejectsInvalidInstalledSchemaAtConstruction(t *testing.T) {
	if _, err := agent.NewResultDecoder([]byte(`{"type":`)); err == nil {
		t.Fatal("invalid JSON Schema was accepted")
	}
}

func TestReconResultDecoderRequiresACompleteMarkdownArtifact(t *testing.T) {
	decoder, err := agent.NewReconResultDecoder([]byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["recon"],
  "properties": {"recon": {"type": "string"}}
}`))
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	got, err := decoder.Decode([]byte(`{"recon":"# Recon\n\n## Issue\n\nAdd the substep.\n"}`))
	if err != nil {
		t.Fatalf("decode recon: %v", err)
	}
	if got.Recon != "# Recon\n\n## Issue\n\nAdd the substep.\n" {
		t.Fatalf("recon = %q", got.Recon)
	}

	for _, input := range []string{
		`{"recon":""}`,
		`{"recon":"not markdown"}`,
		`{"recon":"# Recon\n","extra":true}`,
	} {
		if _, err := decoder.Decode([]byte(input)); err == nil {
			t.Fatalf("invalid recon result was accepted: %s", input)
		}
	}
}
