package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

// MaxIssueSlugLength bounds the title-derived portion of an awdev branch.
const MaxIssueSlugLength = 48

const (
	gitStdoutLimit = 1 << 20
	gitStderrLimit = 64 << 10
)

// WorktreeRequest contains validated source data needed to prepare one issue
// worktree.
type WorktreeRequest struct {
	ControllerRoot     string
	RepositoryIdentity string
	DefaultBranch      string
	IssueNumber        int
	IssueTitle         string
}

// Worktree is the validated, pinned workspace passed to later workflow phases.
type Worktree struct {
	Branch       string
	BaseSHA      string
	AbsolutePath string
}

// WorktreePreparer is the repository-operation seam used by the workflow.
type WorktreePreparer interface {
	Prepare(context.Context, WorktreeRequest) (Worktree, error)
}

// WorktreeManager creates and validates deterministic issue worktrees.
type WorktreeManager struct {
	binary string
	runner processrun.Runner
}

// NewWorktreeManager constructs a Git-backed worktree manager.
func NewWorktreeManager(binary string, runner processrun.Runner) *WorktreeManager {
	return &WorktreeManager{binary: binary, runner: runner}
}

// IssueSlug converts an untrusted issue title to a deterministic ASCII slug.
func IssueSlug(title string) string {
	var slug strings.Builder
	separator := false
	for _, character := range title {
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			if separator && slug.Len() > 0 && slug.Len() < MaxIssueSlugLength {
				slug.WriteByte('-')
			}
			separator = false
			if slug.Len() < MaxIssueSlugLength {
				slug.WriteRune(character)
			}
			continue
		}
		separator = true
	}

	result := strings.TrimRight(slug.String(), "-")
	if result == "" {
		return "issue"
	}
	return result
}

// Prepare verifies the repository, pins the remote base, and either creates or
// exactly reuses the deterministic issue worktree.
func (manager *WorktreeManager) Prepare(ctx context.Context, request WorktreeRequest) (Worktree, error) {
	if err := validateWorktreeRequest(request); err != nil {
		return Worktree{}, err
	}
	if manager.runner == nil || strings.TrimSpace(manager.binary) == "" {
		return Worktree{}, errors.New("Git worktree manager is not fully configured")
	}
	controllerRoot, err := canonicalExistingDirectory(request.ControllerRoot)
	if err != nil {
		return Worktree{}, fmt.Errorf("validate controller root: %w", err)
	}

	origin, err := manager.output(ctx, controllerRoot, "config", "--get", "remote.origin.url")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve required origin remote: %w", err)
	}
	originIdentity, err := githubRepositoryIdentity(origin)
	if err != nil {
		return Worktree{}, fmt.Errorf("validate origin remote: %w", err)
	}
	if !strings.EqualFold(originIdentity, request.RepositoryIdentity) {
		return Worktree{}, fmt.Errorf("origin repository %q does not match GitHub repository %q", originIdentity, request.RepositoryIdentity)
	}

	shallow, err := manager.output(ctx, controllerRoot, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return Worktree{}, fmt.Errorf("detect shallow repository: %w", err)
	}
	if shallow == "true" {
		return Worktree{}, errors.New("shallow controller repositories are unsupported; fetch complete history before running awdev")
	}
	if shallow != "false" {
		return Worktree{}, fmt.Errorf("detect shallow repository: unexpected response %q", shallow)
	}
	worktreeArea, err := controllerWorktreeArea(controllerRoot)
	if err != nil {
		return Worktree{}, err
	}

	remoteRef := "refs/remotes/origin/" + request.DefaultBranch
	refspec := "+refs/heads/" + request.DefaultBranch + ":" + remoteRef
	if err := manager.command(ctx, controllerRoot, "fetch", "--no-tags", "origin", refspec); err != nil {
		return Worktree{}, fmt.Errorf("fetch origin default branch %q: %w", request.DefaultBranch, err)
	}
	baseSHA, err := manager.output(ctx, controllerRoot, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve pinned base %s: %w", remoteRef, err)
	}
	if !isFullObjectID(baseSHA) {
		return Worktree{}, fmt.Errorf("resolve pinned base %s: Git returned malformed object ID %q", remoteRef, baseSHA)
	}

	slug := IssueSlug(request.IssueTitle)
	artifactName := "gh-" + strconv.Itoa(request.IssueNumber) + "-" + slug
	branch := artifactName
	absolutePath := filepath.Join(worktreeArea, artifactName)
	result := Worktree{
		Branch:       branch,
		BaseSHA:      baseSHA,
		AbsolutePath: absolutePath,
	}
	repairedStaleRegistration, err := manager.removeMissingDetachedRegistration(ctx, controllerRoot, absolutePath)
	if err != nil {
		return Worktree{}, err
	}

	matched, err := manager.matchesValidatedWorktree(ctx, controllerRoot, result)
	if err != nil {
		return Worktree{}, err
	}
	if matched {
		return result, nil
	}

	if _, err := os.Lstat(absolutePath); err == nil {
		return Worktree{}, fmt.Errorf("deterministic worktree path %q exists but is not registered; preserving it for diagnosis", absolutePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Worktree{}, fmt.Errorf("inspect deterministic worktree path: %w", err)
	}
	branchExists, err := manager.branchExists(ctx, controllerRoot, branch)
	if err != nil {
		return Worktree{}, err
	}
	if branchExists {
		if !repairedStaleRegistration {
			return Worktree{}, fmt.Errorf("deterministic branch %q already exists without its exact worktree; preserving it for diagnosis", branch)
		}
		branchHead, resolveErr := manager.output(ctx, controllerRoot, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
		if resolveErr != nil {
			return Worktree{}, fmt.Errorf("resolve recovered workflow branch: %w", resolveErr)
		}
		if branchHead != baseSHA {
			return Worktree{}, fmt.Errorf("recovered workflow branch HEAD %q does not match pinned base %q; preserving it for diagnosis", branchHead, baseSHA)
		}
	}

	// Keep the disposable worktree detached and create the workflow branch as a
	// separate ref. If .awdev is deleted manually, stale Git worktree metadata
	// then cannot lock that branch against deletion.
	if err := manager.command(ctx, controllerRoot, "worktree", "add", "--detach", absolutePath, baseSHA); err != nil {
		return Worktree{}, fmt.Errorf("create deterministic worktree: %w", err)
	}
	if !branchExists {
		if err := manager.command(ctx, controllerRoot, "branch", branch, baseSHA); err != nil {
			return Worktree{}, fmt.Errorf("create deterministic workflow branch: %w", err)
		}
	}
	matched, err = manager.matchesValidatedWorktree(ctx, controllerRoot, result)
	if err != nil {
		return Worktree{}, fmt.Errorf("validate created worktree: %w", err)
	}
	if !matched {
		return Worktree{}, errors.New("created worktree is not registered; preserving bootstrap artifacts for diagnosis")
	}
	return result, nil
}

func (manager *WorktreeManager) removeMissingDetachedRegistration(ctx context.Context, controllerRoot, expectedPath string) (bool, error) {
	entries, err := manager.listWorktrees(ctx, controllerRoot)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !samePath(entry.path, expectedPath) || !entry.detached {
			continue
		}
		if _, statErr := os.Lstat(expectedPath); statErr == nil {
			return false, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return false, fmt.Errorf("inspect detached worktree path: %w", statErr)
		}
		if err := manager.command(ctx, controllerRoot, "worktree", "remove", "--force", expectedPath); err != nil {
			return false, fmt.Errorf("remove stale detached worktree registration: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func validateWorktreeRequest(request WorktreeRequest) error {
	if request.ControllerRoot == "" {
		return errors.New("controller root is required")
	}
	if request.IssueNumber <= 0 {
		return errors.New("issue number must be positive")
	}
	if _, err := githubRepositoryIdentity("https://github.com/" + request.RepositoryIdentity); err != nil {
		return errors.New("GitHub repository identity is malformed")
	}
	if !validRefComponentPath(request.DefaultBranch) {
		return errors.New("GitHub default branch is malformed")
	}
	return nil
}

func canonicalExistingDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func controllerWorktreeArea(controllerRoot string) (string, error) {
	area := filepath.Join(controllerRoot, ".awdev", "worktrees")
	canonical, err := canonicalExistingDirectory(area)
	if err != nil {
		return "", fmt.Errorf("validate controller-owned worktree area: %w", err)
	}
	if !samePath(canonical, area) {
		return "", errors.New("controller-owned worktree area must be a real directory inside the controller repository, not a symlink")
	}
	relative, err := filepath.Rel(controllerRoot, area)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("controller-owned worktree area escapes the controller repository")
	}
	return area, nil
}

func githubRepositoryIdentity(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if left, path, found := strings.Cut(remote, ":"); found && !strings.Contains(left, "/") {
		host := left
		if _, possibleHost, hasUser := strings.Cut(left, "@"); hasUser {
			host = possibleHost
		}
		if strings.EqualFold(host, "github.com") {
			return cleanRepositoryPath(path)
		}
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Hostname() == "" || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", fmt.Errorf("origin URL %q is not a GitHub repository URL", remote)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "ssh", "git":
	default:
		return "", fmt.Errorf("origin URL %q uses unsupported scheme", remote)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("origin URL %q contains unexpected query or fragment", remote)
	}
	return cleanRepositoryPath(parsed.EscapedPath())
}

func cleanRepositoryPath(path string) (string, error) {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", errors.New("origin repository path is malformed")
	}
	decoded = strings.TrimPrefix(decoded, "/")
	decoded = strings.TrimSuffix(decoded, "/")
	decoded = strings.TrimSuffix(decoded, ".git")
	parts := strings.Split(decoded, "/")
	if len(parts) != 2 || !validIdentityPart(parts[0]) || !validIdentityPart(parts[1]) {
		return "", errors.New("origin repository identity is malformed")
	}
	return parts[0] + "/" + parts[1], nil
}

func validIdentityPart(value string) bool {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.')
	})
}

func validRefComponentPath(value string) bool {
	if value == "" || value == "@" || strings.HasPrefix(value, "-") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "..") ||
		strings.Contains(value, "//") || strings.Contains(value, "@{") ||
		strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	if strings.ContainsFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func isFullObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

type worktreeEntry struct {
	path     string
	head     string
	branch   string
	detached bool
	bare     bool
}

func (manager *WorktreeManager) listWorktrees(ctx context.Context, root string) ([]worktreeEntry, error) {
	contents, err := manager.output(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list registered worktrees: %w", err)
	}
	entries, err := parseWorktreePorcelain(contents)
	if err != nil {
		return nil, fmt.Errorf("parse registered worktrees: %w", err)
	}
	return entries, nil
}

func parseWorktreePorcelain(contents string) ([]worktreeEntry, error) {
	contents = strings.TrimSpace(contents)
	if contents == "" {
		return nil, errors.New("Git returned no registered worktrees")
	}
	records := strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n\n")
	entries := make([]worktreeEntry, 0, len(records))
	paths := make(map[string]struct{}, len(records))
	branches := make(map[string]struct{}, len(records))
	for _, record := range records {
		var entry worktreeEntry
		for lineNumber, line := range strings.Split(record, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				if lineNumber != 0 || value == "" || entry.path != "" {
					return nil, errors.New("malformed worktree path record")
				}
				if strings.HasPrefix(value, `"`) {
					unquoted, err := strconv.Unquote(value)
					if err != nil {
						return nil, errors.New("malformed escaped worktree path")
					}
					value = unquoted
				}
				entry.path = filepath.Clean(value)
			case "HEAD":
				if entry.head != "" || !isFullObjectID(value) {
					return nil, errors.New("malformed worktree HEAD record")
				}
				entry.head = value
			case "branch":
				if entry.branch != "" || entry.detached || entry.bare || !strings.HasPrefix(value, "refs/heads/") {
					return nil, errors.New("malformed worktree branch record")
				}
				entry.branch = strings.TrimPrefix(value, "refs/heads/")
			case "detached":
				if value != "" || entry.detached || entry.bare || entry.branch != "" {
					return nil, errors.New("contradictory or repeated detached worktree state")
				}
				entry.detached = true
			case "bare":
				if value != "" || entry.bare || entry.detached || entry.branch != "" {
					return nil, errors.New("contradictory or repeated bare worktree state")
				}
				entry.bare = true
			case "locked", "prunable":
				// Informational state does not change identity validation.
			default:
				return nil, fmt.Errorf("unknown porcelain field %q", key)
			}
		}
		if entry.path == "" || entry.head == "" || (entry.branch == "" && !entry.detached && !entry.bare) {
			return nil, errors.New("incomplete worktree record")
		}
		pathKey := filepath.Clean(entry.path)
		if runtime.GOOS == "windows" {
			pathKey = strings.ToLower(pathKey)
		}
		if _, exists := paths[pathKey]; exists {
			return nil, fmt.Errorf("duplicate worktree path %q", entry.path)
		}
		paths[pathKey] = struct{}{}
		if entry.branch != "" {
			if _, exists := branches[entry.branch]; exists {
				return nil, fmt.Errorf("duplicate worktree branch %q", entry.branch)
			}
			branches[entry.branch] = struct{}{}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func findDeterministicWorktree(entries []worktreeEntry, expectedPath, expectedBranch string) (*worktreeEntry, string) {
	var pathMatch, branchMatch *worktreeEntry
	for index := range entries {
		entry := &entries[index]
		if samePath(entry.path, expectedPath) {
			pathMatch = entry
		}
		if entry.branch == expectedBranch {
			branchMatch = entry
		}
	}
	if pathMatch == nil {
		if branchMatch == nil {
			return nil, ""
		}
		return nil, fmt.Sprintf("deterministic branch %q is registered at conflicting path %q; preserving it for diagnosis", expectedBranch, branchMatch.path)
	}
	if !pathMatch.detached && pathMatch.branch != expectedBranch {
		return nil, fmt.Sprintf("deterministic worktree path %q is registered on conflicting branch %q; preserving it for diagnosis", expectedPath, pathMatch.branch)
	}
	if branchMatch != nil && pathMatch != branchMatch {
		return nil, fmt.Sprintf("deterministic branch %q and path %q belong to different worktrees; preserving them for diagnosis", expectedBranch, expectedPath)
	}
	return pathMatch, ""
}

func (manager *WorktreeManager) matchesValidatedWorktree(ctx context.Context, controllerRoot string, expected Worktree) (bool, error) {
	entries, err := manager.listWorktrees(ctx, controllerRoot)
	if err != nil {
		return false, err
	}
	matched, collision := findDeterministicWorktree(entries, expected.AbsolutePath, expected.Branch)
	if collision != "" {
		return false, errors.New(collision)
	}
	if matched == nil {
		return false, nil
	}
	if err := manager.validateWorktree(ctx, controllerRoot, expected, *matched); err != nil {
		return false, err
	}
	return true, nil
}

func samePath(first, second string) bool {
	firstAbsolute, firstErr := filepath.Abs(first)
	secondAbsolute, secondErr := filepath.Abs(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	firstClean := filepath.Clean(firstAbsolute)
	secondClean := filepath.Clean(secondAbsolute)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(firstClean, secondClean)
	}
	return firstClean == secondClean
}

func (manager *WorktreeManager) validateWorktree(ctx context.Context, controllerRoot string, expected Worktree, actual worktreeEntry) error {
	if actual.bare || (!actual.detached && actual.branch != expected.Branch) {
		return fmt.Errorf("deterministic worktree has conflicting branch state; preserving it for diagnosis")
	}
	if actual.head != expected.BaseSHA {
		return fmt.Errorf("deterministic worktree HEAD %q does not match pinned base %q; preserving it for diagnosis", actual.head, expected.BaseSHA)
	}
	canonicalPath, err := canonicalExistingDirectory(expected.AbsolutePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"deterministic worktree state is incomplete:\n- folder: missing (%s)\n- worktree: stale\n- branch: exists (%s)",
				expected.AbsolutePath,
				expected.Branch,
			)
		}
		return fmt.Errorf("validate deterministic worktree path: %w", err)
	}
	if !samePath(canonicalPath, expected.AbsolutePath) {
		return errors.New("deterministic worktree path resolves outside its registered location; preserving it for diagnosis")
	}
	controllerCommon, err := manager.output(ctx, controllerRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve controller repository identity: %w", err)
	}
	worktreeCommon, err := manager.output(ctx, expected.AbsolutePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve worktree repository identity: %w", err)
	}
	if !samePath(controllerCommon, worktreeCommon) {
		return errors.New("deterministic worktree belongs to a different repository; preserving it for diagnosis")
	}
	checkedHead, err := manager.output(ctx, expected.AbsolutePath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("validate deterministic worktree HEAD: %w", err)
	}
	if checkedHead != expected.BaseSHA {
		return fmt.Errorf("deterministic worktree checked-out HEAD %q does not match pinned base %q; preserving it for diagnosis", checkedHead, expected.BaseSHA)
	}
	branchHead, err := manager.output(ctx, controllerRoot, "rev-parse", "--verify", "refs/heads/"+expected.Branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("validate deterministic workflow branch: %w", err)
	}
	if branchHead != expected.BaseSHA {
		return fmt.Errorf("deterministic workflow branch HEAD %q does not match pinned base %q; preserving it for diagnosis", branchHead, expected.BaseSHA)
	}
	if !actual.detached {
		checkedBranch, err := manager.output(ctx, expected.AbsolutePath, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return fmt.Errorf("validate deterministic worktree branch: %w", err)
		}
		if checkedBranch != expected.Branch {
			return fmt.Errorf("deterministic worktree checked-out branch %q does not match %q; preserving it for diagnosis", checkedBranch, expected.Branch)
		}
	}
	return nil
}

func (manager *WorktreeManager) branchExists(ctx context.Context, root, branch string) (bool, error) {
	result, err := manager.run(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitError *processrun.ExitError
	if errors.As(err, &exitError) && result.ExitCode == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect deterministic branch: %w", commandError(result, err))
}

func (manager *WorktreeManager) output(ctx context.Context, root string, arguments ...string) (string, error) {
	result, err := manager.run(ctx, root, arguments...)
	if err != nil {
		return "", commandError(result, err)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func (manager *WorktreeManager) command(ctx context.Context, root string, arguments ...string) error {
	result, err := manager.run(ctx, root, arguments...)
	if err != nil {
		return commandError(result, err)
	}
	return nil
}

func (manager *WorktreeManager) run(ctx context.Context, root string, arguments ...string) (processrun.Result, error) {
	return manager.runner.Run(ctx, processrun.Request{
		Directory: root,
		Argv:      append([]string{manager.binary}, arguments...),
		Environment: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			"NO_COLOR":            "1",
		},
		StdoutLimit: gitStdoutLimit,
		StderrLimit: gitStderrLimit,
	})
}

func commandError(result processrun.Result, err error) error {
	if result.StdoutTruncated || result.StderrTruncated {
		return fmt.Errorf("Git output exceeded the capture limit: %w", err)
	}
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}
