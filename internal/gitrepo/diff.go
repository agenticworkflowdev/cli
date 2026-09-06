package gitrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const diffOutputLimit = 4 << 20

// DiffBaseline captures protected filesystem state immediately before an
// agent receives workspace-write access.
type DiffBaseline struct {
	scope           DiffScope
	gitMarker       map[string]fileSignature
	gitMetadataRoot string
	gitMetadata     map[string]fileSignature
	commonGitRoot   string
	commonGit       map[string]fileSignature
	controller      map[string]fileSignature
	protected       map[string]fileSignature
}

// DiffScope identifies the controller, implementation worktree, and
// repository-configured paths protected from agent writes.
type DiffScope struct {
	ControllerRoot    string
	Worktree          string
	ProtectedPrefixes []string
}

type fileSignature struct {
	mode     fs.FileMode
	size     int64
	modified int64
	digest   [sha256.Size]byte
	target   string
}

// DiffScopeInspector is the post-agent protected-path seam used by workflows.
type DiffScopeInspector interface {
	Capture(DiffScope) (DiffBaseline, error)
	Inspect(context.Context, DiffScope, DiffBaseline) ([]string, error)
}

// DiffInspector finds changed Git paths and protected controller metadata.
type DiffInspector struct {
	binary string
	runner processrun.Runner
}

// NewDiffInspector constructs a Git-backed implementation scope inspector.
func NewDiffInspector(binary string, runner processrun.Runner) *DiffInspector {
	return &DiffInspector{binary: binary, runner: runner}
}

// Capture records the worktree Git marker and controller metadata before an
// agent run. The implementation worktree itself is intentionally excluded.
func (inspector *DiffInspector) Capture(scope DiffScope) (DiffBaseline, error) {
	if err := inspector.validate(scope); err != nil {
		return DiffBaseline{}, err
	}
	controllerRoot := scope.ControllerRoot
	worktree := scope.Worktree
	gitMarker, err := snapshotFilesystem(filepath.Join(worktree, ".git"), ".git", false)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("capture worktree Git metadata: %w", err)
	}
	controller, err := snapshotFilesystem(filepath.Join(controllerRoot, ".awdev"), ".awdev", true)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("capture controller state: %w", err)
	}
	gitMetadataRoot, err := resolveGitMetadataRoot(worktree)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("resolve linked Git metadata: %w", err)
	}
	gitMetadata, err := snapshotGitMetadata(gitMetadataRoot)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("capture linked Git metadata: %w", err)
	}
	commonGitRoot, err := resolveCommonGitRoot(gitMetadataRoot)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("resolve common Git metadata: %w", err)
	}
	commonGit := make(map[string]fileSignature)
	if commonGitRoot != "" && commonGitRoot != gitMetadataRoot {
		commonGit, err = snapshotGitMetadata(commonGitRoot)
		if err != nil {
			return DiffBaseline{}, fmt.Errorf("capture common Git metadata: %w", err)
		}
		removeSnapshotSubtree(commonGit, commonGitRoot, gitMetadataRoot)
	}
	protected, err := snapshotProtectedPaths(worktree, scope.ProtectedPrefixes)
	if err != nil {
		return DiffBaseline{}, fmt.Errorf("capture protected paths: %w", err)
	}
	return DiffBaseline{
		scope:           cloneDiffScope(scope),
		gitMarker:       gitMarker,
		gitMetadataRoot: gitMetadataRoot,
		gitMetadata:     gitMetadata,
		commonGitRoot:   commonGitRoot,
		commonGit:       commonGit,
		controller:      controller,
		protected:       protected,
	}, nil
}

// Inspect reports protected paths changed since the baseline. Allowed source
// changes are left untouched for later checks and review.
func (inspector *DiffInspector) Inspect(ctx context.Context, scope DiffScope, baseline DiffBaseline) ([]string, error) {
	if err := inspector.validate(scope); err != nil {
		return nil, err
	}
	if !sameDiffScope(baseline.scope, scope) {
		return nil, errors.New("diff baseline does not belong to this workflow worktree")
	}
	controllerRoot := scope.ControllerRoot
	worktree := scope.Worktree

	offending := make(map[string]struct{})
	gitMarker, err := snapshotFilesystem(filepath.Join(worktree, ".git"), ".git", false)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree Git metadata: %w", err)
	}
	for _, changed := range changedSnapshotPaths(baseline.gitMarker, gitMarker) {
		offending[changed] = struct{}{}
	}
	gitMetadata, err := snapshotGitMetadata(baseline.gitMetadataRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect linked Git metadata: %w", err)
	}
	for _, changed := range changedSnapshotPaths(baseline.gitMetadata, gitMetadata) {
		offending[changed] = struct{}{}
	}
	if baseline.commonGitRoot != "" && baseline.commonGitRoot != baseline.gitMetadataRoot {
		commonGit, err := snapshotGitMetadata(baseline.commonGitRoot)
		if err != nil {
			return nil, fmt.Errorf("inspect common Git metadata: %w", err)
		}
		removeSnapshotSubtree(commonGit, baseline.commonGitRoot, baseline.gitMetadataRoot)
		for _, changed := range changedSnapshotPaths(baseline.commonGit, commonGit) {
			offending[changed] = struct{}{}
		}
	}
	controller, err := snapshotFilesystem(filepath.Join(controllerRoot, ".awdev"), ".awdev", true)
	if err != nil {
		return nil, fmt.Errorf("inspect controller state: %w", err)
	}
	for _, changed := range changedSnapshotPaths(baseline.controller, controller) {
		offending[changed] = struct{}{}
	}
	protected, err := snapshotProtectedPaths(worktree, scope.ProtectedPrefixes)
	if err != nil {
		return nil, fmt.Errorf("inspect protected paths: %w", err)
	}
	for _, changed := range changedSnapshotPaths(baseline.protected, protected) {
		offending[changed] = struct{}{}
	}

	processResult, runErr := inspector.runner.Run(ctx, processrun.Request{
		Directory:   worktree,
		Argv:        []string{inspector.binary, "status", "--porcelain=v1", "-z", "--untracked-files=all"},
		StdoutLimit: diffOutputLimit,
		StderrLimit: gitStderrLimit,
	})
	if runErr != nil {
		if len(offending) > 0 {
			return sortedPathSet(offending), nil
		}
		return nil, fmt.Errorf("inspect implementation diff: %w", runErr)
	}
	if processResult.StdoutTruncated {
		return nil, errors.New("implementation diff path list exceeded the output limit")
	}
	changedPaths, err := parsePorcelainPaths(processResult.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse implementation diff: %w", err)
	}
	for _, changed := range changedPaths {
		if protectedDiffPath(changed, scope.ProtectedPrefixes) {
			offending[changed] = struct{}{}
		}
	}
	return sortedPathSet(offending), nil
}

func (inspector *DiffInspector) validate(scope DiffScope) error {
	if inspector == nil || inspector.runner == nil || strings.TrimSpace(inspector.binary) == "" {
		return errors.New("diff inspector is not configured")
	}
	for name, value := range map[string]string{"controller root": scope.ControllerRoot, "worktree": scope.Worktree} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("diff %s must be an absolute clean path", name)
		}
	}
	relative, err := filepath.Rel(scope.ControllerRoot, scope.Worktree)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("diff worktree must be beneath the controller root")
	}
	return nil
}

func snapshotFilesystem(absoluteRoot, displayRoot string, excludeWorktrees bool) (map[string]fileSignature, error) {
	excluded := make(map[string]bool)
	if excludeWorktrees {
		excluded["worktrees"] = true
	}
	return snapshotFilesystemExcluding(absoluteRoot, displayRoot, excluded)
}

func snapshotFilesystemExcluding(absoluteRoot, displayRoot string, excludedDirectories map[string]bool) (map[string]fileSignature, error) {
	return snapshotFilesystemWithSigner(absoluteRoot, displayRoot, excludedDirectories, signatureFor)
}

func snapshotFilesystemWithSigner(absoluteRoot, displayRoot string, excludedDirectories map[string]bool, signer func(string, fs.FileInfo) (fileSignature, error)) (map[string]fileSignature, error) {
	snapshot := make(map[string]fileSignature)
	info, err := os.Lstat(absoluteRoot)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		signature, err := signer(absoluteRoot, info)
		if err != nil {
			return nil, err
		}
		snapshot[displayRoot] = signature
		return snapshot, nil
	}

	err = filepath.WalkDir(absoluteRoot, func(absolutePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if absolutePath == absoluteRoot {
			return nil
		}
		relative, err := filepath.Rel(absoluteRoot, absolutePath)
		if err != nil {
			return err
		}
		if excludedDirectories[filepath.ToSlash(relative)] && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		signature, err := signer(absolutePath, info)
		if err != nil {
			return err
		}
		displayPath := path.Join(displayRoot, filepath.ToSlash(relative))
		snapshot[displayPath] = signature
		return nil
	})
	return snapshot, err
}

func signatureFor(absolutePath string, info fs.FileInfo) (fileSignature, error) {
	signature, err := metadataSignatureFor(absolutePath, info)
	if err != nil {
		return fileSignature{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return signature, nil
	}
	if !info.Mode().IsRegular() {
		return signature, nil
	}
	contents, err := os.ReadFile(absolutePath)
	if err != nil {
		return fileSignature{}, err
	}
	signature.digest = sha256.Sum256(contents)
	return signature, nil
}

func metadataSignatureFor(absolutePath string, info fs.FileInfo) (fileSignature, error) {
	signature := fileSignature{mode: info.Mode(), size: info.Size(), modified: info.ModTime().UnixNano()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(absolutePath)
		if err != nil {
			return fileSignature{}, err
		}
		signature.target = target
	}
	return signature, nil
}

func resolveGitMetadataRoot(worktree string) (string, error) {
	markerPath := filepath.Join(worktree, ".git")
	info, err := os.Lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return markerPath, nil
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(contents))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", nil
	}
	gitDirectory := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDirectory) {
		gitDirectory = filepath.Join(worktree, gitDirectory)
	}
	return filepath.Clean(gitDirectory), nil
}

func snapshotGitMetadata(gitDirectory string) (map[string]fileSignature, error) {
	if gitDirectory == "" {
		return map[string]fileSignature{}, nil
	}
	snapshot, err := snapshotFilesystem(gitDirectory, ".git", false)
	if err != nil {
		return nil, err
	}
	// FETCH_HEAD is an ephemeral record of the most recent fetch. Git clients
	// may refresh it concurrently without changing refs or repository state.
	delete(snapshot, ".git/FETCH_HEAD")
	return snapshot, nil
}

func resolveCommonGitRoot(gitDirectory string) (string, error) {
	if gitDirectory == "" {
		return "", nil
	}
	contents, err := os.ReadFile(filepath.Join(gitDirectory, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return gitDirectory, nil
	}
	if err != nil {
		return "", err
	}
	commonDirectory := strings.TrimSpace(string(contents))
	if commonDirectory == "" {
		return "", errors.New("linked Git commondir is empty")
	}
	if !filepath.IsAbs(commonDirectory) {
		commonDirectory = filepath.Join(gitDirectory, commonDirectory)
	}
	return filepath.Clean(commonDirectory), nil
}

func snapshotProtectedPaths(worktree string, configured []string) (map[string]fileSignature, error) {
	protected := make(map[string]fileSignature)
	prefixes := append([]string{".gitmodules"}, configured...)
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(filepath.ToSlash(prefix), "/")
		if _, err := normalizeDiffPath(prefix); err != nil {
			return nil, fmt.Errorf("invalid protected prefix %q", prefix)
		}
		captured, err := snapshotFilesystem(filepath.Join(worktree, filepath.FromSlash(prefix)), prefix, false)
		if err != nil {
			return nil, err
		}
		for name, signature := range captured {
			protected[name] = signature
		}
	}
	return protected, nil
}

func changedSnapshotPaths(before, after map[string]fileSignature) []string {
	changed := make(map[string]struct{})
	for name, previous := range before {
		current, exists := after[name]
		if !exists || current != previous {
			changed[name] = struct{}{}
		}
	}
	for name, current := range after {
		previous, exists := before[name]
		if !exists || current != previous {
			changed[name] = struct{}{}
		}
	}
	return sortedPathSet(changed)
}

func removeSnapshotSubtree(snapshot map[string]fileSignature, snapshotRoot, subtreeRoot string) {
	relative, err := filepath.Rel(snapshotRoot, subtreeRoot)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return
	}
	displayPrefix := path.Join(".git", filepath.ToSlash(relative))
	for name := range snapshot {
		if pathPrefixMatch(name, displayPrefix) {
			delete(snapshot, name)
		}
	}
}

func parsePorcelainPaths(output []byte) ([]string, error) {
	paths := make([]string, 0)
	for offset := 0; offset < len(output); {
		end := offset + bytes.IndexByte(output[offset:], 0)
		if end < offset {
			return nil, errors.New("Git status record is not NUL terminated")
		}
		record := string(output[offset:end])
		offset = end + 1
		if len(record) < 4 || record[2] != ' ' {
			return nil, errors.New("Git status record is malformed")
		}
		changed, err := normalizeDiffPath(record[3:])
		if err != nil {
			return nil, err
		}
		paths = append(paths, changed)
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			end = offset + bytes.IndexByte(output[offset:], 0)
			if end < offset {
				return nil, errors.New("Git rename source is not NUL terminated")
			}
			changed, err = normalizeDiffPath(string(output[offset:end]))
			if err != nil {
				return nil, err
			}
			paths = append(paths, changed)
			offset = end + 1
		}
	}
	return paths, nil
}

func normalizeDiffPath(value string) (string, error) {
	value = filepath.ToSlash(value)
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return "", fmt.Errorf("Git returned unsafe changed path %q", value)
	}
	return value, nil
}

func protectedDiffPath(changed string, configured []string) bool {
	if pathPrefixMatch(changed, ".git") || changed == ".gitmodules" {
		return true
	}
	for _, prefix := range configured {
		if pathPrefixMatch(changed, strings.TrimSuffix(filepath.ToSlash(prefix), "/")) {
			return true
		}
	}
	return false
}

func pathPrefixMatch(changed, prefix string) bool {
	return changed == prefix || strings.HasPrefix(changed, prefix+"/")
}

func sortedPathSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneDiffScope(scope DiffScope) DiffScope {
	scope.ProtectedPrefixes = append([]string(nil), scope.ProtectedPrefixes...)
	return scope
}

func sameDiffScope(left, right DiffScope) bool {
	if left.ControllerRoot != right.ControllerRoot || left.Worktree != right.Worktree || len(left.ProtectedPrefixes) != len(right.ProtectedPrefixes) {
		return false
	}
	for index := range left.ProtectedPrefixes {
		if left.ProtectedPrefixes[index] != right.ProtectedPrefixes[index] {
			return false
		}
	}
	return true
}
