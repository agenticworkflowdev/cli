package diagnostic_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/diagnostic"
)

func TestReportErrorWritesPrivateProjectLogAndPrintsRelativePath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".awdev"), 0o700); err != nil {
		t.Fatal(err)
	}
	workingDirectory := filepath.Join(root, "nested", "directory")
	if err := os.MkdirAll(workingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	logPath, err := diagnostic.ReportError(
		&output,
		workingDirectory,
		[]string{"awdev", "run", "github", "2"},
		detailedError{err: errors.New("codex exited with code 1"), details: "stderr:\nauthentication failed\n"},
	)
	if err != nil {
		t.Fatalf("report error: %v", err)
	}
	if !filepath.IsAbs(logPath) {
		t.Fatalf("log path = %q, want absolute path", logPath)
	}
	wantDirectory := filepath.Join(root, ".awdev", "logs")
	if filepath.Dir(logPath) != wantDirectory {
		t.Fatalf("log directory = %q, want %q", filepath.Dir(logPath), wantDirectory)
	}
	displayPath := filepath.ToSlash(filepath.Join(".awdev", "logs", filepath.Base(logPath)))
	if got := output.String(); !strings.Contains(got, "codex exited with code 1\n") || !strings.Contains(got, "Error log: "+displayPath+"\n") {
		t.Fatalf("terminal output = %q", got)
	}

	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read error log: %v", err)
	}
	for _, want := range []string{
		"working_directory: <repository path outside .awdev>",
		`arguments: ["awdev" "run" "github" "2"]`,
		"error:\ncodex exited with code 1",
		"diagnostics:\nstderr:\nauthentication failed",
	} {
		if !strings.Contains(string(contents), want) {
			t.Errorf("error log does not contain %q:\n%s", want, contents)
		}
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("log permissions = %o, want 600", permissions)
	}
}

func TestReportErrorPersistsControllerPathsOnlyAsAWDevRelativePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".awdev"), 0o700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, ".awdev", "worktrees", "gh-2-title")
	schema := filepath.Join(root, ".awdev", "schemas", "agent-result.schema.json")
	output := filepath.Join(root, ".awdev", "issues", "wf_0123456789abcdef0123456789abcdef", ".agent-final-1.json")
	failure := detailedError{
		err:     errors.New("cannot write " + worktree),
		details: "argv: [\"codex\" \"--cd\" \"" + worktree + "\" \"--output-schema\" \"" + schema + "\"]\nstderr:\nfailed to open " + output + "\n",
	}

	logPath, err := diagnostic.ReportError(io.Discard, root, []string{"awdev", "run", "github", "2"}, failure)
	if err != nil {
		t.Fatalf("report error: %v", err)
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read error log: %v", err)
	}
	text := string(contents)
	if strings.Contains(text, root) {
		t.Fatalf("error log contains absolute controller path:\n%s", text)
	}
	for _, want := range []string{
		".awdev/worktrees/gh-2-title",
		".awdev/schemas/agent-result.schema.json",
		".awdev/issues/wf_0123456789abcdef0123456789abcdef/.agent-final-1.json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("error log does not contain relative path %q:\n%s", want, text)
		}
	}
}

func TestReportErrorPrintsControllerOwnedPathsRelativeToTheRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".awdev"), 0o700); err != nil {
		t.Fatal(err)
	}
	absoluteWorktree := filepath.Join(root, ".awdev", "worktrees", "gh-2-title")
	var output bytes.Buffer
	if _, err := diagnostic.ReportError(&output, root, []string{"awdev", "run", "github", "2"}, errors.New("missing "+absoluteWorktree)); err != nil {
		t.Fatalf("report error: %v", err)
	}
	if got := output.String(); strings.Contains(got, root) || !strings.Contains(got, "missing .awdev/worktrees/gh-2-title") {
		t.Fatalf("terminal output contains a non-relative controller path: %q", got)
	}
}

func TestReportErrorRecreatesProjectLogDirectoryWhenAWDevWasDeleted(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workingDirectory := filepath.Join(root, "nested")
	if err := os.Mkdir(workingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	logPath, err := diagnostic.ReportError(&output, workingDirectory, []string{"awdev", "run", "github", "2"}, errors.New("missing configuration"))
	if err != nil {
		t.Fatalf("report error: %v", err)
	}
	wantDirectory := filepath.Join(root, ".awdev", "logs")
	if filepath.Dir(logPath) != wantDirectory {
		t.Fatalf("log directory = %q, want %q", filepath.Dir(logPath), wantDirectory)
	}
	displayPath := filepath.ToSlash(filepath.Join(".awdev", "logs", filepath.Base(logPath)))
	if got := output.String(); strings.Contains(got, root) || !strings.Contains(got, "Error log: "+displayPath) {
		t.Fatalf("terminal output = %q, want repository-relative log path", got)
	}
}

type detailedError struct {
	err     error
	details string
}

func (failure detailedError) Error() string             { return failure.err.Error() }
func (failure detailedError) Unwrap() error             { return failure.err }
func (failure detailedError) DiagnosticDetails() string { return failure.details }
