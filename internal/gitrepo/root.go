// Package gitrepo provides Git repository discovery and operations.
package gitrepo

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// DiscoverControllerRoot returns the original checkout root for the Git work
// tree containing cwd. From a linked worktree it returns the checkout that owns
// the common .git directory.
func DiscoverControllerRoot(ctx context.Context, cwd string) (string, error) {
	inside, err := gitOutput(ctx, cwd, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", err
	}
	if inside != "true" {
		return "", fmt.Errorf("%q is not inside a Git work tree", cwd)
	}

	commonDirectory, err := gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if filepath.Base(commonDirectory) == ".git" {
		return filepath.Clean(filepath.Dir(commonDirectory)), nil
	}

	root, err := gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return strings.TrimSpace(string(output)), nil
}
