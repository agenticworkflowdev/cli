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
	".awdev/prompts",
	".awdev/schemas",
	".awdev/worktrees",
}

var ignoreEntries = []string{
	".awdev/issues/",
	".awdev/locks/",
	".awdev/worktrees/",
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
}

// Initialize installs missing defaults beneath controllerRoot without replacing
// or rewriting any existing repository-owned asset.
func Initialize(controllerRoot string, provider agent.Provider) (Result, error) {
	if provider != agent.ProviderCodex {
		return Result{}, fmt.Errorf("unsupported agent %q", provider)
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

	err := fs.WalkDir(assets.Defaults(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
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

	result.Gitignore, err = updateIgnoreFile(filepath.Join(controllerRoot, ".gitignore"))
	if err != nil {
		return Result{}, fmt.Errorf("update .gitignore: %w", err)
	}

	return result, nil
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
