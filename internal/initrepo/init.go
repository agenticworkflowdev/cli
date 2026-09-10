// Package initrepo additively installs repository-owned awdev defaults.
package initrepo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/assets"
)

var managedDirectories = []string{
	".awdev",
	".awdev/issues",
	".awdev/locks",
	".awdev/logs",
	".awdev/prompts",
	".awdev/schemas",
	".awdev/worktrees",
}

var ignoreEntries = []string{
	".awdev/issues/",
	".awdev/locks/",
	".awdev/logs/",
	".awdev/worktrees/",
}

type skillInstallation struct {
	source      string
	destination string
}

var skillInstallations = map[agent.Provider][]skillInstallation{
	agent.ProviderCodex: {
		{source: "SKILL.md", destination: ".agents/skills/awdev/SKILL.md"},
		{source: "agents/openai.yaml", destination: ".agents/skills/awdev/agents/openai.yaml"},
	},
	agent.ProviderClaudeCode: {
		{source: "SKILL.md", destination: ".claude/skills/awdev/SKILL.md"},
	},
}

// Options controls optional repository integrations installed by Initialize.
type Options struct {
	WithSkill bool
}

// FileStatus describes how initialization handled an optional file.
type FileStatus string

const (
	FileCreated  FileStatus = "created"
	FileRetained FileStatus = "retained"
)

// SkillFileResult contains the outcome for one optional repository skill file.
type SkillFileResult struct {
	Path   string
	Status FileStatus
}

// SkillResult contains the provider-specific files installed for the optional
// repository agent skill.
type SkillResult struct {
	Files []SkillFileResult
}

// GitignoreStatus describes how initialization handled .gitignore.
type GitignoreStatus string

const (
	GitignoreCreated  GitignoreStatus = "created"
	GitignoreRetained GitignoreStatus = "retained"
	GitignoreUpdated  GitignoreStatus = "updated"
)

// Result contains only the repository-level outcomes presented to the user.
type Result struct {
	AwdevDirectoryCreated bool
	Gitignore             GitignoreStatus
	Skill                 *SkillResult
}

// Initialize installs missing defaults beneath controllerRoot without replacing
// or rewriting any existing repository-owned asset.
func Initialize(controllerRoot string, provider agent.Provider, options Options) (Result, error) {
	if provider != agent.ProviderCodex && provider != agent.ProviderClaudeCode {
		return Result{}, fmt.Errorf("unsupported agent %q", provider)
	}
	defaultConfig, err := assets.DefaultConfig(provider)
	if err != nil {
		return Result{}, err
	}
	var result Result
	for _, relative := range managedDirectories {
		created, err := ensureDirectory(filepath.Join(controllerRoot, filepath.FromSlash(relative)))
		if err != nil {
			return Result{}, fmt.Errorf("prepare %s: %w", relative, err)
		}
		if relative == ".awdev" {
			result.AwdevDirectoryCreated = created
		}
	}

	if _, err := createFileIfMissing(filepath.Join(controllerRoot, ".awdev", "config.json"), defaultConfig); err != nil {
		return Result{}, fmt.Errorf("install .awdev/config.json: %w", err)
	}

	err = fs.WalkDir(assets.Defaults(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if isConfigTemplate(path) {
			return nil
		}
		contents, err := fs.ReadFile(assets.Defaults(), path)
		if err != nil {
			return err
		}
		relative := filepath.ToSlash(filepath.Join(".awdev", filepath.FromSlash(path)))
		_, err = createFileIfMissing(filepath.Join(controllerRoot, filepath.FromSlash(relative)), contents)
		if err != nil {
			return fmt.Errorf("install %s: %w", relative, err)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if options.WithSkill {
		skill, err := installSkill(controllerRoot, provider)
		if err != nil {
			return Result{}, err
		}
		result.Skill = &skill
	}

	result.Gitignore, err = updateIgnoreFile(filepath.Join(controllerRoot, ".gitignore"))
	if err != nil {
		return Result{}, fmt.Errorf("update .gitignore: %w", err)
	}

	return result, nil
}

func isConfigTemplate(path string) bool {
	base := filepath.Base(filepath.ToSlash(path))
	return strings.HasPrefix(base, "config") && strings.HasSuffix(base, ".json")
}

func installSkill(controllerRoot string, provider agent.Provider) (SkillResult, error) {
	installations, ok := skillInstallations[provider]
	if !ok {
		return SkillResult{}, fmt.Errorf("no repository skill installation for agent provider %q", provider)
	}
	result := SkillResult{Files: make([]SkillFileResult, 0, len(installations))}
	for _, installation := range installations {
		if err := ensureParentDirectories(controllerRoot, installation.destination); err != nil {
			return SkillResult{}, err
		}
		contents, err := fs.ReadFile(assets.RepositorySkill(), installation.source)
		if err != nil {
			return SkillResult{}, fmt.Errorf("read repository skill asset %s: %w", installation.source, err)
		}
		created, err := createFileIfMissing(filepath.Join(controllerRoot, filepath.FromSlash(installation.destination)), contents)
		if err != nil {
			return SkillResult{}, fmt.Errorf("install %s: %w", installation.destination, err)
		}
		status := FileRetained
		if created {
			status = FileCreated
		}
		result.Files = append(result.Files, SkillFileResult{Path: installation.destination, Status: status})
	}
	return result, nil
}

func ensureParentDirectories(controllerRoot, filePath string) error {
	relativeDirectory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(filePath)))
	current := controllerRoot
	currentRelative := ""
	for _, name := range strings.Split(relativeDirectory, "/") {
		current = filepath.Join(current, name)
		currentRelative = filepath.Join(currentRelative, name)
		if _, err := ensureDirectory(current); err != nil {
			return fmt.Errorf("prepare %s: %w", filepath.ToSlash(currentRelative), err)
		}
	}
	return nil
}

func ensureDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("existing path is not a directory")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

func createFileIfMissing(path string, contents []byte) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("existing path is not a regular file")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	removeIncomplete := true
	defer func() {
		if removeIncomplete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	removeIncomplete = false
	return true, nil
}

func updateIgnoreFile(path string) (GitignoreStatus, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		contents := []byte(strings.Join(ignoreEntries, "\n") + "\n")
		created, createErr := createFileIfMissing(path, contents)
		if createErr != nil {
			return GitignoreRetained, createErr
		}
		if created {
			return GitignoreCreated, nil
		}
		return updateIgnoreFile(path)
	}
	if err != nil {
		return GitignoreRetained, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return GitignoreRetained, err
	}
	if !info.Mode().IsRegular() {
		return GitignoreRetained, fmt.Errorf("existing path is not a regular file")
	}

	missing := make([]string, 0, len(ignoreEntries))
	for _, entry := range ignoreEntries {
		if !containsLine(contents, entry) {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return GitignoreRetained, nil
	}

	var addition strings.Builder
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		addition.WriteByte('\n')
	}
	for _, entry := range missing {
		addition.WriteString(entry)
		addition.WriteByte('\n')
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return GitignoreRetained, err
	}
	if _, err := file.WriteString(addition.String()); err != nil {
		_ = file.Close()
		return GitignoreRetained, err
	}
	if err := file.Close(); err != nil {
		return GitignoreRetained, err
	}
	return GitignoreUpdated, nil
}

func containsLine(contents []byte, want string) bool {
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.TrimSuffix(line, "\r") == want {
			return true
		}
	}
	return false
}
