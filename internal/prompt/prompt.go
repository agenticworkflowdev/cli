// Package prompt compiles and renders repository-owned prompt templates.
package prompt

import (
	"bytes"
	"fmt"
	"text/template"
)

// IssueData is the persisted issue snapshot exposed to prompt templates.
type IssueData struct {
	Number int
	Title  string
	Body   string
	URL    string
}

// PromptData is the typed controller context exposed to prompt templates.
type PromptData struct {
	WorkflowID        string
	Repository        string
	Branch            string
	BaseSHA           string
	SpecificationPath string
	Issue             IssueData
}

// Renderer is one prompt compiled exactly once with strict missing-key
// behavior.
type Renderer struct {
	template *template.Template
}

// NewRenderer compiles an installed prompt template.
func NewRenderer(name string, contents []byte) (*Renderer, error) {
	compiled, err := template.New(name).Option("missingkey=error").Parse(string(contents))
	if err != nil {
		return nil, fmt.Errorf("compile %s prompt: %w", name, err)
	}
	return &Renderer{template: compiled}, nil
}

// Render executes the already-compiled template with typed data. Rendered
// bytes are never parsed as template source.
func (renderer *Renderer) Render(data PromptData) (string, error) {
	if renderer == nil || renderer.template == nil {
		return "", fmt.Errorf("prompt renderer is not configured")
	}
	var output bytes.Buffer
	if err := renderer.template.Execute(&output, data); err != nil {
		return "", fmt.Errorf("render %s prompt: %w", renderer.template.Name(), err)
	}
	return output.String(), nil
}
