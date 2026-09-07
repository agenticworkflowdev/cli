// Package review defines and validates independent review evidence.
package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const resultSchemaLocation = "urn:awdev:review-result-schema"

// Severity ranks an actionable review finding.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Finding is one semantically validated, structured correction request.
type Finding struct {
	Severity Severity `json:"severity"`
	Path     string   `json:"path,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

// Result is the complete output of one independent review invocation.
type Result struct {
	Approved bool      `json:"approved"`
	Findings []Finding `json:"findings"`
}

// Validate rejects review combinations that a JSON schema cannot express.
func (result Result) Validate() error {
	if result.Findings == nil {
		return errors.New("review findings must be an array")
	}
	if result.Approved && len(result.Findings) != 0 {
		return errors.New("approved review must not contain findings")
	}
	if !result.Approved && len(result.Findings) == 0 {
		return errors.New("rejected review must contain findings")
	}
	for index, finding := range result.Findings {
		switch finding.Severity {
		case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		default:
			return fmt.Errorf("finding %d severity is invalid", index)
		}
		if strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("finding %d message must not be blank", index)
		}
		if finding.Path != "" && !validPath(finding.Path) {
			return fmt.Errorf("finding %d path must be repository-relative", index)
		}
		if finding.Line < 0 {
			return fmt.Errorf("finding %d line must be positive", index)
		}
		if finding.Line > 0 && finding.Path == "" {
			return fmt.Errorf("finding %d line requires a path", index)
		}
	}
	return nil
}

func validPath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || filepath.IsAbs(value) {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

// ResultDecoder applies the installed JSON schema before strict typed and
// semantic validation.
type ResultDecoder struct {
	schema *jsonschema.Schema
}

// NewResultDecoder compiles one repository-owned review-result schema.
func NewResultDecoder(contents []byte) (*ResultDecoder, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode review result schema: %w", err)
	}
	if err := requireSingleJSONDocument(decoder, "review result schema"); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resultSchemaLocation, document); err != nil {
		return nil, fmt.Errorf("load review result schema: %w", err)
	}
	compiled, err := compiler.Compile(resultSchemaLocation)
	if err != nil {
		return nil, fmt.Errorf("compile review result schema: %w", err)
	}
	return &ResultDecoder{schema: compiled}, nil
}

// Decode validates and returns one complete review result.
func (decoder *ResultDecoder) Decode(contents []byte) (Result, error) {
	if decoder == nil || decoder.schema == nil {
		return Result{}, errors.New("review result decoder is not configured")
	}
	var document any
	generic := json.NewDecoder(bytes.NewReader(contents))
	generic.UseNumber()
	if err := generic.Decode(&document); err != nil {
		return Result{}, fmt.Errorf("decode review result JSON: %w", err)
	}
	if err := requireSingleJSONDocument(generic, "review result"); err != nil {
		return Result{}, err
	}
	if err := decoder.schema.Validate(document); err != nil {
		return Result{}, fmt.Errorf("validate review result schema: %w", err)
	}

	strict := json.NewDecoder(bytes.NewReader(contents))
	strict.DisallowUnknownFields()
	var result Result
	if err := strict.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode typed review result: %w", err)
	}
	if err := requireSingleJSONDocument(strict, "review result"); err != nil {
		return Result{}, err
	}
	if err := result.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate review result semantics: %w", err)
	}
	return result, nil
}

func requireSingleJSONDocument(decoder *json.Decoder, name string) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s must contain a single JSON object", name)
	}
	return nil
}
