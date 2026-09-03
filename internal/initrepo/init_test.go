package initrepo_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
)

var expectedLayout = []string{
	".awdev",
	".awdev/config.json",
	".awdev/issues",
	".awdev/locks",
	".awdev/prompts",
	".awdev/prompts/fix-checks.md",
	".awdev/prompts/fix-review.md",
	".awdev/prompts/implement.md",
	".awdev/prompts/review.md",
	".awdev/prompts/spec.md",
	".awdev/schemas",
	".awdev/schemas/agent-result.schema.json",
	".awdev/schemas/review-result.schema.json",
	".awdev/worktrees",
	".gitignore",
}

func TestInitializeCreatesCompleteLayoutAndIsIdempotent(t *testing.T) {
	root := t.TempDir()

	first, err := initrepo.Initialize(root, agent.ProviderCodex)
	if err != nil {
		t.Fatalf("first initialize: %v", err)
	}
	if !first.AwdevDirectoryCreated || first.Gitignore != initrepo.GitignoreCreated {
		t.Fatalf("first result = %#v, want created .awdev and .gitignore", first)
	}
	if got := listLayout(t, root); !reflect.DeepEqual(got, expectedLayout) {
		t.Fatalf("layout:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(expectedLayout, "\n"))
	}
	golden := snapshotFiles(t, filepath.Join("testdata", "golden"))
	if got := snapshotFiles(t, root); !reflect.DeepEqual(got, golden) {
		t.Fatalf("initialized file bytes differ from testdata/golden\ngot: %#v\nwant: %#v", got, golden)
	}

	before := snapshotFiles(t, root)
	second, err := initrepo.Initialize(root, agent.ProviderCodex)
	if err != nil {
		t.Fatalf("second initialize: %v", err)
	}
	after := snapshotFiles(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("second initialization changed file bytes")
	}
	if second.AwdevDirectoryCreated || second.Gitignore != initrepo.GitignoreRetained {
		t.Fatalf("second result = %#v, want retained .awdev and .gitignore", second)
	}
}

func TestInitializePreservesExistingAssetsByteForByte(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, ".awdev", "prompts", "spec.md")
	configPath := filepath.Join(root, ".awdev", "config.json")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		t.Fatalf("create prompt directory: %v", err)
	}
	wantPrompt := []byte("custom prompt\x00without newline")
	wantConfig := []byte(`{"locally":"edited"}`)
	if err := os.WriteFile(promptPath, wantPrompt, 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := os.WriteFile(configPath, wantConfig, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := initrepo.Initialize(root, agent.ProviderCodex)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	assertFileBytes(t, promptPath, wantPrompt)
	assertFileBytes(t, configPath, wantConfig)
}

func TestInitializeAppendsOnlyMissingIgnoreEntries(t *testing.T) {
	tests := []struct {
		name     string
		existing *string
		want     string
		status   initrepo.GitignoreStatus
	}{
		{name: "absent", want: ".awdev/issues/\n.awdev/locks/\n.awdev/worktrees/\n", status: initrepo.GitignoreCreated},
		{name: "partial", existing: stringPointer("bin/\n.awdev/issues/\n"), want: "bin/\n.awdev/issues/\n.awdev/locks/\n.awdev/worktrees/\n", status: initrepo.GitignoreUpdated},
		{name: "duplicates", existing: stringPointer(".awdev/issues/\n.awdev/issues/\n.awdev/locks/\n.awdev/worktrees/\n"), want: ".awdev/issues/\n.awdev/issues/\n.awdev/locks/\n.awdev/worktrees/\n", status: initrepo.GitignoreRetained},
		{name: "missing final newline", existing: stringPointer("bin/"), want: "bin/\n.awdev/issues/\n.awdev/locks/\n.awdev/worktrees/\n", status: initrepo.GitignoreUpdated},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.existing != nil {
				if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(*test.existing), 0o644); err != nil {
					t.Fatalf("write ignore file: %v", err)
				}
			}

			result, err := initrepo.Initialize(root, agent.ProviderCodex)
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			assertFileBytes(t, filepath.Join(root, ".gitignore"), []byte(test.want))
			if result.Gitignore != test.status {
				t.Errorf("gitignore status = %q, want %q", result.Gitignore, test.status)
			}
		})
	}
}

func listLayout(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk layout: %v", err)
	}
	sort.Strings(paths)
	return paths
}

func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	for _, path := range listLayout(t, root) {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		snapshot[path] = string(contents)
	}
	return snapshot
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s bytes = %q, want %q", path, got, want)
	}
}

func stringPointer(value string) *string {
	return &value
}
