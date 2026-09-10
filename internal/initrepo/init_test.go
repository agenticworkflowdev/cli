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
	".awdev/logs",
	".awdev/prompts",
	".awdev/prompts/fix-checks.md",
	".awdev/prompts/fix-review.md",
	".awdev/prompts/implement.md",
	".awdev/prompts/recon.md",
	".awdev/prompts/resume-blocked.md",
	".awdev/prompts/review.md",
	".awdev/prompts/spec.md",
	".awdev/schemas",
	".awdev/schemas/agent-result.schema.json",
	".awdev/schemas/recon-result.schema.json",
	".awdev/schemas/review-result.schema.json",
	".awdev/worktrees",
	".gitignore",
}

type providerCase struct {
	name       string
	provider   agent.Provider
	goldenDir  string
	skillPaths []string
}

var providerCases = []providerCase{
	{
		name:      "codex",
		provider:  agent.ProviderCodex,
		goldenDir: "codex",
		skillPaths: []string{
			".agents/skills/awdev/SKILL.md",
			".agents/skills/awdev/agents/openai.yaml",
		},
	},
	{
		name:       "claude-code",
		provider:   agent.ProviderClaudeCode,
		goldenDir:  "claude-code",
		skillPaths: []string{".claude/skills/awdev/SKILL.md"},
	},
}

func skillLayout(pc providerCase) []string {
	entries := make(map[string]struct{})
	for _, skillPath := range pc.skillPaths {
		entries[skillPath] = struct{}{}
		for directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(skillPath))); directory != "."; directory = filepath.ToSlash(filepath.Dir(filepath.FromSlash(directory))) {
			entries[directory] = struct{}{}
		}
	}
	paths := make([]string, 0, len(entries))
	for entry := range entries {
		paths = append(paths, entry)
	}
	sort.Strings(paths)
	return paths
}

func TestInitializeCreatesCompleteLayoutAndIsIdempotent(t *testing.T) {
	for _, pc := range providerCases {
		t.Run(pc.name, func(t *testing.T) {
			root := t.TempDir()

			first, err := initrepo.Initialize(root, pc.provider, initrepo.Options{})
			if err != nil {
				t.Fatalf("first initialize: %v", err)
			}
			if !first.AwdevDirectoryCreated || first.Gitignore != initrepo.GitignoreCreated {
				t.Fatalf("first result = %#v, want created .awdev and .gitignore", first)
			}
			if got := listLayout(t, root); !reflect.DeepEqual(got, expectedLayout) {
				t.Fatalf("layout:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(expectedLayout, "\n"))
			}
			golden := snapshotFiles(t, filepath.Join("testdata", "golden", pc.goldenDir))
			if got := snapshotFiles(t, root); !reflect.DeepEqual(got, golden) {
				t.Fatalf("initialized file bytes differ from testdata/golden/%s\ngot: %#v\nwant: %#v", pc.goldenDir, got, golden)
			}

			before := snapshotFiles(t, root)
			second, err := initrepo.Initialize(root, pc.provider, initrepo.Options{})
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
		})
	}
}

func TestInitializeRejectsUnsupportedProvider(t *testing.T) {
	if _, err := initrepo.Initialize(t.TempDir(), agent.Provider("bogus"), initrepo.Options{}); err == nil {
		t.Fatal("initialize accepted an unsupported agent provider")
	}
}

func TestInitializePreservesExistingAssetsByteForByte(t *testing.T) {
	for _, pc := range providerCases {
		t.Run(pc.name, func(t *testing.T) {
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

			_, err := initrepo.Initialize(root, pc.provider, initrepo.Options{})
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			assertFileBytes(t, promptPath, wantPrompt)
			assertFileBytes(t, configPath, wantConfig)
		})
	}
}

func TestInitializeWithSkillCreatesOptionalFilesAndIsIdempotent(t *testing.T) {
	for _, pc := range providerCases {
		t.Run(pc.name, func(t *testing.T) {
			root := t.TempDir()

			first, err := initrepo.Initialize(root, pc.provider, initrepo.Options{WithSkill: true})
			if err != nil {
				t.Fatalf("first initialize: %v", err)
			}
			wantFirst := skillFileResults(pc.skillPaths, initrepo.FileCreated)
			if first.Skill == nil || !reflect.DeepEqual(first.Skill.Files, wantFirst) {
				t.Fatalf("first skill result = %#v, want %#v", first.Skill, wantFirst)
			}
			for _, path := range skillLayout(pc) {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
					t.Errorf("stat %s: %v", path, err)
				}
			}
			before := snapshotFiles(t, root)

			second, err := initrepo.Initialize(root, pc.provider, initrepo.Options{WithSkill: true})
			if err != nil {
				t.Fatalf("second initialize: %v", err)
			}
			wantSecond := skillFileResults(pc.skillPaths, initrepo.FileRetained)
			if second.Skill == nil || !reflect.DeepEqual(second.Skill.Files, wantSecond) {
				t.Fatalf("second skill result = %#v, want %#v", second.Skill, wantSecond)
			}
			if after := snapshotFiles(t, root); !reflect.DeepEqual(after, before) {
				t.Fatal("second opt-in initialization changed file bytes")
			}
		})
	}
}

func TestInitializeWithSkillInstallsClaudeCodeSkillInClaudeProjectDirectory(t *testing.T) {
	root := t.TempDir()

	if _, err := initrepo.Initialize(root, agent.ProviderClaudeCode, initrepo.Options{WithSkill: true}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "awdev", "SKILL.md")); err != nil {
		t.Fatalf("stat Claude Code project skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("generic agent directory exists after Claude Code skill installation: %v", err)
	}
}

func TestInitializeWithSkillAddsOnlyMissingFilesAndPreservesExistingBytes(t *testing.T) {
	for _, pc := range providerCases {
		for _, existingRelative := range pc.skillPaths {
			t.Run(pc.name+"/"+existingRelative, func(t *testing.T) {
				root := t.TempDir()
				existingPath := filepath.Join(root, filepath.FromSlash(existingRelative))
				if err := os.MkdirAll(filepath.Dir(existingPath), 0o755); err != nil {
					t.Fatalf("create existing file directory: %v", err)
				}
				wantExisting := []byte("locally edited\x00without newline")
				if err := os.WriteFile(existingPath, wantExisting, 0o600); err != nil {
					t.Fatalf("write existing file: %v", err)
				}

				result, err := initrepo.Initialize(root, pc.provider, initrepo.Options{WithSkill: true})
				if err != nil {
					t.Fatalf("initialize: %v", err)
				}
				wantFiles := skillFileResults(pc.skillPaths, initrepo.FileCreated)
				for index := range wantFiles {
					if wantFiles[index].Path == existingRelative {
						wantFiles[index].Status = initrepo.FileRetained
					}
				}
				if result.Skill == nil || !reflect.DeepEqual(result.Skill.Files, wantFiles) {
					t.Fatalf("skill result = %#v, want %#v", result.Skill, wantFiles)
				}
				assertFileBytes(t, existingPath, wantExisting)
				for _, relative := range pc.skillPaths {
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err != nil {
						t.Fatalf("stat installed %s: %v", relative, err)
					}
				}
			})
		}
	}
}

func TestInstalledSkillIsAnExplicitOnlyCLIDispatcher(t *testing.T) {
	for _, pc := range providerCases {
		t.Run(pc.name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := initrepo.Initialize(root, pc.provider, initrepo.Options{WithSkill: true}); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			instructions, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pc.skillPaths[0])))
			if err != nil {
				t.Fatalf("read instructions: %v", err)
			}
			if !strings.Contains(string(instructions), "disable-model-invocation: true") {
				t.Error("instructions do not require explicit invocation")
			}
			for _, command := range []string{"awdev run github NUMBER", "awdev status github NUMBER", "awdev resume github NUMBER"} {
				if !strings.Contains(string(instructions), command) {
					t.Errorf("instructions do not contain %q", command)
				}
			}
			for _, legacy := range []string{"awdev run NUMBER", "awdev status NUMBER", "awdev resume NUMBER", "awdev retry"} {
				if strings.Contains(string(instructions), legacy) {
					t.Errorf("instructions contain unsupported command %q", legacy)
				}
			}
			for _, claim := range []string{"daemon", "MCP", "Linear", "Claude Code"} {
				if strings.Contains(string(instructions), claim) {
					t.Errorf("instructions contain out-of-scope claim %q", claim)
				}
			}
			if len(pc.skillPaths) > 1 {
				metadata, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pc.skillPaths[1])))
				if err != nil {
					t.Fatalf("read metadata: %v", err)
				}
				wantMetadata := "interface:\n" +
					"  display_name: \"AWDev\"\n" +
					"  short_description: \"Dispatch an AWDev GitHub issue workflow\"\n" +
					"  default_prompt: \"Use $awdev to dispatch an AWDev GitHub issue workflow.\"\n" +
					"policy:\n" +
					"  allow_implicit_invocation: false\n"
				if string(metadata) != wantMetadata {
					t.Fatalf("metadata = %q, want approved explicit-only fixture %q", metadata, wantMetadata)
				}
			}
		})
	}
}

func TestInitializeAppendsOnlyMissingIgnoreEntries(t *testing.T) {
	tests := []struct {
		name     string
		existing *string
		want     string
		status   initrepo.GitignoreStatus
	}{
		{name: "absent", want: ".awdev/issues/\n.awdev/locks/\n.awdev/logs/\n.awdev/worktrees/\n", status: initrepo.GitignoreCreated},
		{name: "partial", existing: stringPointer("bin/\n.awdev/issues/\n"), want: "bin/\n.awdev/issues/\n.awdev/locks/\n.awdev/logs/\n.awdev/worktrees/\n", status: initrepo.GitignoreUpdated},
		{name: "duplicates", existing: stringPointer(".awdev/issues/\n.awdev/issues/\n.awdev/locks/\n.awdev/logs/\n.awdev/worktrees/\n"), want: ".awdev/issues/\n.awdev/issues/\n.awdev/locks/\n.awdev/logs/\n.awdev/worktrees/\n", status: initrepo.GitignoreRetained},
		{name: "missing final newline", existing: stringPointer("bin/"), want: "bin/\n.awdev/issues/\n.awdev/locks/\n.awdev/logs/\n.awdev/worktrees/\n", status: initrepo.GitignoreUpdated},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.existing != nil {
				if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(*test.existing), 0o644); err != nil {
					t.Fatalf("write ignore file: %v", err)
				}
			}

			result, err := initrepo.Initialize(root, agent.ProviderCodex, initrepo.Options{})
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

func skillFileResults(paths []string, status initrepo.FileStatus) []initrepo.SkillFileResult {
	results := make([]initrepo.SkillFileResult, 0, len(paths))
	for _, path := range paths {
		results = append(results, initrepo.SkillFileResult{Path: path, Status: status})
	}
	return results
}

func stringPointer(value string) *string {
	return &value
}
