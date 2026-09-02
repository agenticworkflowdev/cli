package app_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/app"
	"github.com/agenticworkflowdev/cli/internal/config"
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
