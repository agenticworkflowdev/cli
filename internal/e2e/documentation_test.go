package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseDocumentationUsesSourceAwareCommandsAndStatesBoundaries(t *testing.T) {
	list := exec.Command("git", "ls-files", "*.md")
	list.Dir = repositoryRoot
	output, err := list.Output()
	if err != nil {
		t.Fatalf("list tracked documentation: %v", err)
	}
	paths := strings.Fields(string(output))
	paths = append(paths, "README.md")
	legacy := regexp.MustCompile(`(?m)^\s*(?:go run \./cmd/awdev |\./bin/)?awdev (?:run|status|resume|retry) [0-9]+(?:\s|$)`)
	prohibitedClaims := []*regexp.Regexp{
		regexp.MustCompile(`(?i)python (?:runtime|worker) (?:is|required|powers)`),
		regexp.MustCompile(`(?i)(?:daemon|hosted execution|MCP integration) (?:is|are) (?:implemented|supported)`),
		regexp.MustCompile(`(?i)(?:supports|implements) (?:Linear|Claude Code)`),
	}
	var combined strings.Builder
	for _, relative := range paths {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if match := legacy.Find(contents); match != nil {
			t.Errorf("%s contains legacy unscoped command %q", relative, match)
		}
		for _, prohibited := range prohibitedClaims {
			if match := prohibited.Find(contents); match != nil {
				t.Errorf("%s contains obsolete capability claim %q", relative, match)
			}
		}
		combined.Write(contents)
		combined.WriteByte('\n')
	}

	text := combined.String()
	for _, command := range []string{"awdev run github 123", "awdev status github 123", "awdev resume github 123", "awdev retry github 123"} {
		if !strings.Contains(text, command) {
			t.Errorf("release documentation does not contain %q", command)
		}
	}
	if !strings.Contains(text, "awdev run github 123 --skip-spec") {
		t.Error("release documentation does not document the --skip-spec run option")
	}
	for _, statement := range []string{
		"GitHub is the implemented issue source",
		"Linear operations are unavailable",
		"non-shallow",
		".awdev/issues/<workflow-id>/manifest.json",
		"pull_request/failed",
	} {
		if !strings.Contains(text, statement) {
			t.Errorf("release documentation does not state %q", statement)
		}
	}
	// The agent-provider boundary wording is owned by the documentation wave;
	// assert the invariant tolerantly rather than pinning an exact sentence:
	// both Codex and Claude Code must be named as implemented/selectable agents.
	for _, agentBoundary := range []*regexp.Regexp{
		regexp.MustCompile(`(?is)codex.*claude code.*implemented|implemented agent.*codex`),
		regexp.MustCompile(`(?i)claude code`),
	} {
		if !agentBoundary.MatchString(text) {
			t.Errorf("release documentation does not describe the Codex/Claude Code agent boundary (pattern %q)", agentBoundary)
		}
	}
}

func TestBuiltHelpUsesSourceAwareGrammar(t *testing.T) {
	for _, operation := range []string{"run", "status", "resume", "retry"} {
		command := exec.Command(awdevBinary, operation, "--help")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("awdev %s --help: %v\n%s", operation, err, output)
		}
		if !strings.Contains(string(output), "awdev "+operation+" github NUMBER") {
			t.Errorf("%s help lacks source-aware example:\n%s", operation, output)
		}
		if operation == "run" && !strings.Contains(string(output), "--skip-spec") {
			t.Errorf("run help lacks --skip-spec option:\n%s", output)
		}
	}
}
