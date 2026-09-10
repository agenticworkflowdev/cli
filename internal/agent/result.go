package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	resultSchemaLocation      = "urn:awdev:agent-result-schema"
	reconResultSchemaLocation = "urn:awdev:recon-result-schema"
)

// ResultDecoder validates final output against the installed JSON Schema and
// then decodes and validates the provider-neutral domain result.
type ResultDecoder struct {
	schema *jsonschema.Schema
}

// NewResultDecoder compiles an installed agent-result schema once.
func NewResultDecoder(contents []byte) (*ResultDecoder, error) {
	compiled, err := compileResultSchema(contents, "agent result schema", resultSchemaLocation)
	if err != nil {
		return nil, err
	}
	return &ResultDecoder{schema: compiled}, nil
}

func compileResultSchema(contents []byte, name, location string) (*jsonschema.Schema, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if err := requireSingleJSONDocument(decoder, name); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", name, err)
	}
	return compiled, nil
}

// Decode applies schema validation before strict typed decoding and semantic
// validation.
func (decoder *ResultDecoder) Decode(contents []byte) (Outcome, error) {
	if decoder == nil || decoder.schema == nil {
		return Outcome{}, errors.New("agent result decoder is not configured")
	}
	var document any
	generic := json.NewDecoder(bytes.NewReader(contents))
	generic.UseNumber()
	if err := generic.Decode(&document); err != nil {
		return Outcome{}, fmt.Errorf("decode agent result JSON: %w", err)
	}
	if err := requireSingleJSONDocument(generic, "agent result"); err != nil {
		return Outcome{}, err
	}
	if err := decoder.schema.Validate(document); err != nil {
		return Outcome{}, fmt.Errorf("validate agent result schema: %w", err)
	}

	strict := json.NewDecoder(bytes.NewReader(contents))
	strict.DisallowUnknownFields()
	var outcome Outcome
	if err := strict.Decode(&outcome); err != nil {
		return Outcome{}, fmt.Errorf("decode typed agent result: %w", err)
	}
	if err := requireSingleJSONDocument(strict, "agent result"); err != nil {
		return Outcome{}, err
	}
	if err := outcome.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("validate agent result semantics: %w", err)
	}
	return outcome, nil
}

// Validate rejects impossible status/field combinations independently of the
// installed schema.
func (outcome Outcome) Validate() error {
	switch outcome.Status {
	case OutcomeCompleted:
		if strings.TrimSpace(outcome.Summary) == "" || outcome.Question != "" {
			return errors.New("completed result requires only a non-empty summary")
		}
	case OutcomeBlocked:
		if strings.TrimSpace(outcome.Question) == "" || outcome.Summary != "" {
			return errors.New("blocked result requires only a non-empty question")
		}
	default:
		return fmt.Errorf("unsupported result status %q", outcome.Status)
	}
	return nil
}

// ReconOutcome is the provider-neutral result of one reconnaissance turn.
type ReconOutcome struct {
	Recon string `json:"recon"`
}

// ReconResultDecoder validates and decodes a reconnaissance artifact result.
type ReconResultDecoder struct {
	schema *jsonschema.Schema
}

// NewReconResultDecoder compiles the installed reconnaissance result schema.
func NewReconResultDecoder(contents []byte) (*ReconResultDecoder, error) {
	compiled, err := compileResultSchema(contents, "recon result schema", reconResultSchemaLocation)
	if err != nil {
		return nil, err
	}
	return &ReconResultDecoder{schema: compiled}, nil
}

// Decode applies schema validation and requires a useful Markdown artifact.
func (decoder *ReconResultDecoder) Decode(contents []byte) (ReconOutcome, error) {
	if decoder == nil || decoder.schema == nil {
		return ReconOutcome{}, errors.New("recon result decoder is not configured")
	}
	var document any
	generic := json.NewDecoder(bytes.NewReader(contents))
	generic.UseNumber()
	if err := generic.Decode(&document); err != nil {
		return ReconOutcome{}, fmt.Errorf("decode recon result JSON: %w", err)
	}
	if err := requireSingleJSONDocument(generic, "recon result"); err != nil {
		return ReconOutcome{}, err
	}
	if err := decoder.schema.Validate(document); err != nil {
		return ReconOutcome{}, fmt.Errorf("validate recon result schema: %w", err)
	}

	strict := json.NewDecoder(bytes.NewReader(contents))
	strict.DisallowUnknownFields()
	var outcome ReconOutcome
	if err := strict.Decode(&outcome); err != nil {
		return ReconOutcome{}, fmt.Errorf("decode typed recon result: %w", err)
	}
	if err := requireSingleJSONDocument(strict, "recon result"); err != nil {
		return ReconOutcome{}, err
	}
	trimmed := strings.TrimSpace(outcome.Recon)
	if !strings.HasPrefix(trimmed, "# Recon\n") || strings.TrimSpace(strings.TrimPrefix(trimmed, "# Recon")) == "" {
		return ReconOutcome{}, errors.New("recon result must contain a non-empty Markdown artifact headed by # Recon")
	}
	return outcome, nil
}

func requireSingleJSONDocument(decoder *json.Decoder, name string) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s must contain a single JSON object", name)
	}
	return nil
}
