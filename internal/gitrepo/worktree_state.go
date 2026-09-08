package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

// WorktreeBaseline is an opaque snapshot of every tracked and untracked path
// visible to Git at one point in time.
type WorktreeBaseline struct {
	worktree string
	files    map[string]fileSignature
	gitState []byte
}

// WorktreeStateInspector is the filesystem-immutability seam used around
// nominally read-only agent runs.
type WorktreeStateInspector interface {
	Capture(context.Context, string) (WorktreeBaseline, error)
	Inspect(context.Context, string, WorktreeBaseline) ([]string, error)
}

// WorktreeInspector snapshots tracked and untracked worktree contents.
type WorktreeInspector struct {
	binary string
	runner processrun.Runner
}

// NewWorktreeInspector constructs a Git-backed worktree state inspector.
func NewWorktreeInspector(binary string, runner processrun.Runner) *WorktreeInspector {
	return &WorktreeInspector{binary: binary, runner: runner}
}

// Capture records the contents, type, mode, and symlink target of all tracked
// and untracked paths, including ignored files.
func (inspector *WorktreeInspector) Capture(ctx context.Context, worktree string) (WorktreeBaseline, error) {
	files, err := inspector.snapshot(ctx, worktree)
	if err != nil {
		return WorktreeBaseline{}, err
	}
	gitState, err := inspector.snapshotGitState(ctx, worktree)
	if err != nil {
		return WorktreeBaseline{}, err
	}
	return WorktreeBaseline{worktree: worktree, files: files, gitState: gitState}, nil
}

// Inspect returns every path whose worktree state differs from the baseline.
func (inspector *WorktreeInspector) Inspect(ctx context.Context, worktree string, baseline WorktreeBaseline) ([]string, error) {
	if baseline.worktree != worktree || baseline.files == nil {
		return nil, errors.New("worktree baseline does not belong to this worktree")
	}
	current, err := inspector.snapshot(ctx, worktree)
	if err != nil {
		return nil, err
	}
	changed := changedSnapshotPaths(baseline.files, current)
	gitState, err := inspector.snapshotGitState(ctx, worktree)
	if err != nil {
		return nil, err
	}
	// File mutations already identify the affected paths. Report .git only when
	// refs or the index changed without a corresponding filesystem mutation,
	// such as an agent staging or committing the approved contents.
	if !bytes.Equal(baseline.gitState, gitState) && len(changed) == 0 {
		changed = append(changed, ".git")
	}
	sort.Strings(changed)
	return changed, nil
}

func (inspector *WorktreeInspector) snapshotGitState(ctx context.Context, worktree string) ([]byte, error) {
	head, err := inspector.runGitStateCommand(ctx, worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("capture worktree HEAD: %w", err)
	}
	ref, err := inspector.runGitStateCommand(ctx, worktree, "symbolic-ref", "--quiet", "HEAD")
	if isExitCode(err, 1) {
		ref = nil
	} else if err != nil {
		return nil, fmt.Errorf("capture worktree branch ref: %w", err)
	}
	index, err := inspector.runGitStateCommand(ctx, worktree, "ls-files", "--stage", "-z", "--")
	if err != nil {
		return nil, fmt.Errorf("capture worktree index: %w", err)
	}
	indexFlags, err := inspector.runGitStateCommand(ctx, worktree, "ls-files", "-v", "-z", "--")
	if err != nil {
		return nil, fmt.Errorf("capture worktree index flags: %w", err)
	}
	state := make([]byte, 0, len(head)+len(ref)+len(index)+len(indexFlags)+4)
	state = append(state, bytes.TrimSpace(head)...)
	state = append(state, 0)
	state = append(state, bytes.TrimSpace(ref)...)
	state = append(state, 0)
	state = append(state, index...)
	state = append(state, 0)
	state = append(state, indexFlags...)
	return state, nil
}

func (inspector *WorktreeInspector) runGitStateCommand(ctx context.Context, worktree string, arguments ...string) ([]byte, error) {
	result, err := inspector.runner.Run(ctx, processrun.Request{
		Directory:   worktree,
		Argv:        append([]string{inspector.binary}, arguments...),
		StdoutLimit: diffOutputLimit,
		StderrLimit: gitStderrLimit,
	})
	if err != nil {
		return nil, err
	}
	if result.StdoutTruncated {
		return nil, errors.New("worktree Git state exceeded the output limit")
	}
	return append([]byte(nil), result.Stdout...), nil
}

func (inspector *WorktreeInspector) snapshot(ctx context.Context, worktree string) (map[string]fileSignature, error) {
	if inspector == nil || inspector.runner == nil || strings.TrimSpace(inspector.binary) == "" {
		return nil, errors.New("worktree inspector is not configured")
	}
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree {
		return nil, errors.New("worktree path must be absolute and clean")
	}
	info, err := os.Stat(worktree)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("worktree path must be a directory")
	}

	result, err := inspector.runner.Run(ctx, processrun.Request{
		Directory:   worktree,
		Argv:        []string{inspector.binary, "ls-files", "--cached", "--others", "-z", "--"},
		StdoutLimit: diffOutputLimit,
		StderrLimit: gitStderrLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list tracked and untracked worktree paths: %w", err)
	}
	if result.StdoutTruncated {
		return nil, errors.New("worktree path list exceeded the output limit")
	}

	paths, err := parseSnapshotPaths(result.Stdout)
	if err != nil {
		return nil, err
	}
	files := make(map[string]fileSignature, len(paths))
	for _, relative := range paths {
		absolute := filepath.Join(worktree, filepath.FromSlash(relative))
		fileInfo, statErr := os.Lstat(absolute)
		if errors.Is(statErr, os.ErrNotExist) {
			files[relative] = fileSignature{}
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect worktree path %q: %w", relative, statErr)
		}
		signature, err := signatureFor(absolute, fileInfo)
		if err != nil {
			return nil, fmt.Errorf("snapshot worktree path %q: %w", relative, err)
		}
		files[relative] = signature
	}
	if err := filepath.WalkDir(worktree, func(absolute string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if absolute == worktree {
			return nil
		}
		relative, err := filepath.Rel(worktree, absolute)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		signature, err := signatureFor(absolute, fileInfo)
		if err != nil {
			return err
		}
		files[relative] = signature
		return nil
	}); err != nil {
		return nil, fmt.Errorf("snapshot complete worktree filesystem: %w", err)
	}
	return files, nil
}

func parseSnapshotPaths(output []byte) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git worktree path list was not NUL terminated")
	}
	items := bytes.Split(output[:len(output)-1], []byte{0})
	paths := make([]string, 0, len(items))
	for _, item := range items {
		relative := string(item)
		if relative == "" || path.Clean(relative) != relative || path.IsAbs(relative) || strings.ContainsRune(relative, '\x00') || relative == ".." || strings.HasPrefix(relative, "../") {
			return nil, fmt.Errorf("Git returned invalid worktree path %q", relative)
		}
		paths = append(paths, relative)
	}
	return paths, nil
}
