package review_test

import (
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/review"
)

const resultSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["approved", "findings"],
  "properties": {
    "approved": {"type": "boolean"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["severity", "message"],
        "properties": {
          "severity": {"type": "string", "enum": ["low", "medium", "high", "critical"]},
          "path": {"type": "string"},
          "line": {"type": "integer", "minimum": 1},
          "message": {"type": "string", "minLength": 1}
        }
      }
    }
  }
}`

func TestResultDecoderAcceptsOnlySemanticallyConsistentResults(t *testing.T) {
	decoder, err := review.NewResultDecoder([]byte(resultSchema))
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	tests := []struct {
		name     string
		input    string
		approved bool
		wantErr  string
	}{
		{name: "approved", input: `{"approved":true,"findings":[]}`, approved: true},
		{name: "rejected", input: `{"approved":false,"findings":[{"severity":"high","path":"internal/run.go","line":17,"message":"The failure is ignored."}]}`},
		{name: "approved with findings", input: `{"approved":true,"findings":[{"severity":"low","message":"Remove this."}]}`, wantErr: "approved review must not contain findings"},
		{name: "rejected without findings", input: `{"approved":false,"findings":[]}`, wantErr: "rejected review must contain findings"},
		{name: "null findings", input: `{"approved":true,"findings":null}`, wantErr: "schema"},
		{name: "invalid path", input: `{"approved":false,"findings":[{"severity":"medium","path":"../outside","message":"Bad path."}]}`, wantErr: "repository-relative"},
		{name: "line without path", input: `{"approved":false,"findings":[{"severity":"medium","line":3,"message":"Bad line."}]}`, wantErr: "line requires a path"},
		{name: "blank message", input: `{"approved":false,"findings":[{"severity":"low","message":"   "}]}`, wantErr: "message must not be blank"},
		{name: "schema invalid", input: `{"approved":false,"findings":[{"severity":"urgent","message":"Bad."}]}`, wantErr: "schema"},
		{name: "unknown field", input: `{"approved":true,"findings":[],"extra":true}`, wantErr: "schema"},
		{name: "trailing document", input: `{"approved":true,"findings":[]} {}`, wantErr: "single JSON object"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decoder.Decode([]byte(test.input))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Decode() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Approved != test.approved {
				t.Fatalf("approved = %t, want %t", got.Approved, test.approved)
			}
		})
	}
}

func TestResultDecoderRejectsInvalidInstalledSchema(t *testing.T) {
	if _, err := review.NewResultDecoder([]byte(`{"type":`)); err == nil {
		t.Fatal("invalid schema was accepted")
	}
}
