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

const resultSchemaLocation = "urn:awdev:agent-result-schema"

// ResultDecoder validates final output against the installed JSON Schema and
// then decodes and validates the provider-neutral domain result.
type ResultDecoder struct {
	schema *jsonschema.Schema
}

// NewResultDecoder compiles an installed agent-result schema once.
func NewResultDecoder(contents []byte) (*ResultDecoder, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode agent result schema: %w", err)
	}
	if err := requireSingleJSONDocument(decoder, "agent result schema"); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resultSchemaLocation, document); err != nil {
		return nil, fmt.Errorf("load agent result schema: %w", err)
	}
	compiled, err := compiler.Compile(resultSchemaLocation)
	if err != nil {
		return nil, fmt.Errorf("compile agent result schema: %w", err)
	}
	return &ResultDecoder{schema: compiled}, nil
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

func requireSingleJSONDocument(decoder *json.Decoder, name string) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s must contain a single JSON object", name)
	}
	return nil
}
