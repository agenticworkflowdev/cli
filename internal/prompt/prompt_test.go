package prompt_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/prompt"
)

func TestDefaultSpecificationPromptMatchesGolden(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "prompts/spec.md")
	if err != nil {
		t.Fatalf("read default prompt: %v", err)
	}
	renderer, err := prompt.NewRenderer("spec", contents)
	if err != nil {
		t.Fatalf("compile prompt: %v", err)
	}
	got, err := renderer.Render(prompt.PromptData{
		WorkflowID:        "wf_0123456789abcdef0123456789abcdef",
		Repository:        "owner/repository",
		Branch:            "gh-17-a-title",
		BaseSHA:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SpecificationPath: ".awdev/specs/gh-17-a-title.md",
		Issue: prompt.IssueData{
			Number: 17,
			URL:    "https://github.com/owner/repository/issues/17",
			Title:  "Unicode — {{.WorkflowID}}",
			Body:   "line one\n<!-- marker -->\nline three",
		},
	})
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "spec.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("rendered prompt:\n%s\nwant:\n%s", got, want)
	}
}

func TestRendererTreatsIssueFieldsAsLiteralData(t *testing.T) {
	templateText := "{{.Issue.Title}}\n---\n{{.Issue.Body}}\n---\n{{.SpecificationPath}}\n"
	renderer, err := prompt.NewRenderer("spec", []byte(templateText))
	if err != nil {
		t.Fatalf("compile prompt: %v", err)
	}
	large := strings.Repeat("large — <!-- marker --> {{.WorkflowID}}\n", 4096)
	data := prompt.PromptData{
		WorkflowID:        "wf_0123456789abcdef0123456789abcdef",
		SpecificationPath: ".awdev/specs/gh-17-a-title.md",
		Issue: prompt.IssueData{
			Title: "Unicode — {{printf \"executed\"}}",
			Body:  "first line\nsecond line\n" + large,
		},
	}

	got, err := renderer.Render(data)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	for _, literal := range []string{"Unicode — {{printf \"executed\"}}", "<!-- marker -->", "{{.WorkflowID}}", "first line\nsecond line"} {
		if !strings.Contains(got, literal) {
			t.Errorf("rendered prompt does not contain literal %q", literal)
		}
	}
	if strings.Count(got, "large —") != 4096 {
		t.Fatalf("large issue body was truncated: got %d repetitions", strings.Count(got, "large —"))
	}
}

func TestRendererRejectsMissingPromptKeysAtRenderTime(t *testing.T) {
	renderer, err := prompt.NewRenderer("spec", []byte("{{.Missing}}"))
	if err != nil {
		t.Fatalf("compile prompt: %v", err)
	}
	if _, err := renderer.Render(prompt.PromptData{}); err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("render error = %v, want missing key", err)
	}
}

func TestRendererRejectsMalformedTemplateAtConstruction(t *testing.T) {
	if _, err := prompt.NewRenderer("spec", []byte("{{")); err == nil {
		t.Fatal("malformed prompt template was accepted")
	}
}
