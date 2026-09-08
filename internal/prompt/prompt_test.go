package prompt_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/checks"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/review"
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

func TestRendererExposesTypedCheckResultsWithoutReinterpretingOutput(t *testing.T) {
	renderer, err := prompt.NewRenderer("fix-checks", []byte(`{{range .CheckResults}}{{.Name}}|{{.Directory}}|{{printf "%q" .Command}}|{{.ExitCode}}|{{.Duration}}|{{.TimedOut}}|{{.StdoutTruncated}}|{{.StderrTruncated}}
BEGIN STDOUT
{{.Stdout}}
END STDOUT
BEGIN STDERR
{{.Stderr}}
END STDERR
{{end}}`))
	if err != nil {
		t.Fatal(err)
	}
	data := prompt.PromptData{CheckResults: []checks.Result{{
		Name: "unit", Directory: "services/api", Command: []string{"go", "test", "./..."}, ExitCode: 1,
		Duration: 1500 * time.Millisecond, Stdout: "{{.WorkflowID}}\n<!-- output -->", Stderr: "failure\nline two",
		StdoutTruncated: true,
	}}}

	got, err := renderer.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`unit|services/api|["go" "test" "./..."]|1|1.5s|false|true|false`, "{{.WorkflowID}}\n<!-- output -->", "failure\nline two"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered result does not contain %q:\n%s", want, got)
		}
	}
}

func TestDefaultReviewPromptIncludesPinnedSpecAndCompleteCheckEvidence(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "prompts/review.md")
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := prompt.NewRenderer("review", contents)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderer.Render(prompt.PromptData{
		SpecificationPath: ".awdev/specs/gh-17-title.md",
		BaseSHA:           strings.Repeat("a", 40),
		Branch:            "gh-17-title",
		CheckResults: []checks.Result{{
			Name: "unit", Command: []string{"go", "test", "./..."}, ExitCode: 0,
			Stdout: "literal {{.WorkflowID}}", Duration: time.Second,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".awdev/specs/gh-17-title.md", strings.Repeat("a", 40), "gh-17-title", "unit", `["go" "test" "./..."]`, "literal {{.WorkflowID}}"} {
		if !strings.Contains(got, want) {
			t.Errorf("review prompt does not contain %q:\n%s", want, got)
		}
	}
}

func TestDefaultCorrectionPromptIncludesStructuredFindingsAsLiteralData(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "prompts/fix-review.md")
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := prompt.NewRenderer("fix-review", contents)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderer.Render(prompt.PromptData{
		SpecificationPath: ".awdev/specs/gh-17-title.md",
		BaseSHA:           strings.Repeat("b", 40),
		Branch:            "gh-17-title",
		ReviewFindings: []review.Finding{{
			Severity: review.SeverityHigh, Path: "internal/run.go", Line: 42,
			Message: "literal {{.WorkflowID}}\n<!-- review data -->",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".awdev/specs/gh-17-title.md", strings.Repeat("b", 40), "gh-17-title", "high", "internal/run.go", "42", `literal {{.WorkflowID}}\n<!-- review data -->`} {
		if !strings.Contains(got, want) {
			t.Errorf("correction prompt does not contain %q:\n%s", want, got)
		}
	}
}

func TestDefaultResumePromptCarriesQuestionAndAnswerAsQuotedUntrustedData(t *testing.T) {
	contents, err := fs.ReadFile(assets.Defaults(), "prompts/resume-blocked.md")
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := prompt.NewRenderer("resume-blocked", contents)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderer.Render(prompt.PromptData{
		BaseSHA: strings.Repeat("c", 40), Branch: "gh-17-title", SpecificationPath: ".awdev/specs/gh-17-title.md",
		Blocker: &prompt.BlockerData{
			Phase: "implementation", Question: "Which API?\n{{.WorkflowID}}", QuestionURL: "https://github.com/owner/repository/issues/17#issuecomment-100",
			Answer: "Use option A\n<!-- data -->", AnswerAuthor: "human", AnswerURL: "https://github.com/owner/repository/issues/17#issuecomment-101",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Question: "Which API?\n{{.WorkflowID}}"`, `Answer: "Use option A\n<!-- data -->"`, `Answer author: "human"`, "BEGIN UNTRUSTED BLOCKER DATA", "implementation"} {
		if !strings.Contains(got, want) {
			t.Errorf("resume prompt does not contain %q:\n%s", want, got)
		}
	}
}
