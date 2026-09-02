package app_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/app"
	"github.com/agenticworkflowdev/cli/internal/config"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
)

func TestInitCommandInitializesOriginalRepositoryFromNestedDirectory(t *testing.T) {
	t.Setenv("TERM", "dumb")
	repository := t.TempDir()
	command := exec.Command("git", "init", "--quiet", repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	t.Chdir(nested)

	var output bytes.Buffer
	root := app.NewCommand()
	root.SetIn(strings.NewReader("2\n"))
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"init"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute init: %v", err)
	}

	configuration, err := config.Load(repository)
	if err != nil {
		t.Fatalf("load initialized config: %v", err)
	}
	if configuration.Agent.Provider != agent.ProviderCodex {
		t.Fatalf("configured provider = %q, want codex", configuration.Agent.Provider)
	}
	for _, want := range []string{"Select the AI:", "Created .awdev/.", "Created .gitignore", "Initialization complete. awdev is configured to use Codex."} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("init output %q does not contain %q", output.String(), want)
		}
	}
	if strings.Contains(output.String(), ".awdev/config.json") {
		t.Errorf("init output lists internal files: %q", output.String())
	}
}

func TestInitRejectsInvalidExistingConfigBeforeFilesystemChanges(t *testing.T) {
	t.Setenv("TERM", "dumb")
	repository := t.TempDir()
	command := exec.Command("git", "init", "--quiet", repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	awdevDirectory := filepath.Join(repository, ".awdev")
	if err := os.Mkdir(awdevDirectory, 0o755); err != nil {
		t.Fatalf("create .awdev: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awdevDirectory, "config.json"), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("custom-entry"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	before := filesystemSnapshot(t, repository)
	t.Chdir(repository)

	root := app.NewCommand()
	root.SetIn(strings.NewReader("2\n"))
	root.SetOut(new(bytes.Buffer))
	root.SetArgs([]string{"init"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "unsupported schema_version 99") {
		t.Fatalf("execute init error = %v, want invalid existing config", err)
	}

	after := filesystemSnapshot(t, repository)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("filesystem changed after invalid config\nbefore: %#v\nafter: %#v", before, after)
	}
}

func TestRunGitHubCreatesValidatedWorktreeWithoutManifest(t *testing.T) {
	repository := t.TempDir()
	command := exec.Command("git", "init", "--quiet", repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	runGitIn(t, repository, "config", "user.email", "test@example.com")
	runGitIn(t, repository, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, repository, "add", "README.md")
	runGitIn(t, repository, "commit", "--quiet", "-m", "base")
	runGitIn(t, repository, "branch", "-M", "main")
	baseSHA := gitOutputIn(t, repository, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command("git", "init", "--bare", "--quiet", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, output)
	}
	remoteURL := "https://github.com/owner/repository.git"
	runGitIn(t, repository, "remote", "add", "origin", remoteURL)
	runGitIn(t, repository, "config", "url.file://"+filepath.ToSlash(bare)+"/.insteadOf", remoteURL)
	runGitIn(t, repository, "push", "--quiet", "-u", "origin", "main")
	if _, err := initrepo.Initialize(repository, agent.ProviderCodex); err != nil {
		t.Fatalf("initialize repository: %v", err)
	}

	binDirectory := t.TempDir()
	ghPath := filepath.Join(binDirectory, "gh")
	ghScript := `#!/bin/sh
case "$1 $2" in
  "repo view") printf '%s' '{"nameWithOwner":"owner/repository","defaultBranchRef":{"name":"main"}}' ;;
  "api user") printf '%s' '{"login":"octocat"}' ;;
  "issue view") printf '%s' '{"number":17,"title":"A title","body":"body with $() ; and <!-- marker -->","url":"https://github.com/owner/repository/issues/17","state":"OPEN","updatedAt":"2026-08-28T12:00:00Z"}' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(ghScript), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(repository)

	var output bytes.Buffer
	root := app.NewCommand()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"run", "github", "17"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	for _, want := range []string{"Prepared worktree gh-17-a-title", "pinned to " + baseSHA, "for GitHub issue owner/repository#17"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, want substring %q", output.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(repository, ".awdev", "issues", "gh-17", "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest unexpectedly exists: %v", err)
	}
	worktree := filepath.Join(repository, ".awdev", "worktrees", "gh-17-a-title")
	if got := gitOutputIn(t, worktree, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("worktree HEAD = %q, want %q", got, baseSHA)
	}
	if got := gitOutputIn(t, worktree, "branch", "--show-current"); got != "gh-17-a-title" {
		t.Fatalf("worktree branch = %q", got)
	}
	if got := gitOutputIn(t, repository, "branch", "--show-current"); got != "main" {
		t.Fatalf("controller branch changed to %q", got)
	}
}

func TestRunGitHubReportsExistingManifestWithoutGitHub(t *testing.T) {
	repository := t.TempDir()
	command := exec.Command("git", "init", "--quiet", repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if _, err := initrepo.Initialize(repository, agent.ProviderCodex); err != nil {
		t.Fatalf("initialize repository: %v", err)
	}
	stateDirectory := filepath.Join(repository, ".awdev", "issues", "gh-17")
	if err := os.MkdirAll(stateDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "manifest.json"), []byte(`{"phase":"implementation","status":"failed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDirectory := t.TempDir()
	githubCalled := filepath.Join(binDirectory, "github-called")
	ghPath := filepath.Join(binDirectory, "gh")
	ghScript := "#!/bin/sh\nprintf called > \"$AWDEV_GITHUB_CALLED\"\nexit 99\n"
	if err := os.WriteFile(ghPath, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWDEV_GITHUB_CALLED", githubCalled)
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(repository)

	var output bytes.Buffer
	root := app.NewCommand()
	root.SetOut(&output)
	root.SetArgs([]string{"run", "github", "17"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute duplicate run: %v", err)
	}
	if got, want := output.String(), "Workflow gh-17 already exists: implementation/failed.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if _, err := os.Stat(githubCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub was called for an existing manifest: %v", err)
	}
}

func filesystemSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || entry.IsDir() && entry.Name() == ".git" {
			if path != root {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[filepath.ToSlash(relative)+"/"] = "<directory>"
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot filesystem: %v", err)
	}
	return snapshot
}

func runGitIn(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func gitOutputIn(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
