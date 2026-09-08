package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

// PublicationRequest identifies the exact branch and content the controller
// is allowed to commit.
type PublicationRequest struct {
	Worktree          string
	Branch            string
	BaseSHA           string
	SpecificationPath string
	CommitMessage     string
	ExpectedTreeSHA   string
	ExpectedCommitSHA string
}

// PublicationRepository is the controller-owned Git publication boundary.
type PublicationRepository interface {
	Stage(context.Context, PublicationRequest) (string, error)
	Commit(context.Context, PublicationRequest) (string, error)
	Push(context.Context, string, string) error
}

// PublicationManager stages, commits, and pushes through explicit Git argv.
type PublicationManager struct {
	git *WorktreeManager
}

// NewPublicationManager constructs a controller-owned Git publisher.
func NewPublicationManager(binary string, runner processrun.Runner) *PublicationManager {
	return &PublicationManager{git: NewWorktreeManager(binary, runner)}
}

// Stage attaches the initially detached worktree to its recorded branch,
// stages the complete diff, and returns the exact Git tree to persist as commit
// intent before the branch ref can advance.
func (manager *PublicationManager) Stage(ctx context.Context, request PublicationRequest) (string, error) {
	if err := validatePublicationRequest(request); err != nil {
		return "", err
	}
	if err := manager.validate(); err != nil {
		return "", err
	}

	branchResult, branchErr := manager.git.run(ctx, request.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
	switch {
	case branchErr == nil:
		if branch := strings.TrimSpace(string(branchResult.Stdout)); branch != request.Branch {
			return "", fmt.Errorf("worktree is on branch %q, expected recorded branch %q", branch, request.Branch)
		}
	case isExitCode(branchErr, 1):
		head, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve detached worktree HEAD: %w", err)
		}
		branchHead, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "refs/heads/"+request.Branch+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve recorded branch: %w", err)
		}
		if head != request.BaseSHA || branchHead != request.BaseSHA {
			return "", errors.New("detached worktree and recorded branch must both be at the pinned base before publication")
		}
		if err := manager.git.command(ctx, request.Worktree, "checkout", "--quiet", request.Branch); err != nil {
			return "", fmt.Errorf("attach recorded branch: %w", err)
		}
	default:
		return "", fmt.Errorf("resolve current worktree branch: %w", commandError(branchResult, branchErr))
	}

	head, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve staging HEAD: %w", err)
	}
	if head != request.BaseSHA || request.ExpectedCommitSHA != "" {
		return "", errors.New("workflow branch advanced before controller staging")
	}
	if err := manager.git.command(ctx, request.Worktree, "add", "--all", "--", "."); err != nil {
		return "", fmt.Errorf("stage publication changes: %w", err)
	}
	if changed, err := manager.diffChanged(ctx, request.Worktree, "--cached", request.BaseSHA, "--"); err != nil {
		return "", fmt.Errorf("verify staged publication diff: %w", err)
	} else if !changed {
		return "", errors.New("publication requires at least one change from the pinned base")
	}
	if changed, err := manager.diffChanged(ctx, request.Worktree, "--cached", request.BaseSHA, "--", request.SpecificationPath); err != nil {
		return "", fmt.Errorf("verify staged specification: %w", err)
	} else if !changed {
		return "", errors.New("publication diff must include the generated specification")
	}
	treeSHA, err := manager.git.output(ctx, request.Worktree, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write staged publication tree: %w", err)
	}
	if !isFullObjectID(treeSHA) {
		return "", errors.New("Git returned a malformed staged publication tree")
	}
	if request.ExpectedTreeSHA != "" && treeSHA != request.ExpectedTreeSHA {
		return "", errors.New("staged publication tree does not match durable commit intent")
	}
	return treeSHA, nil
}

// Commit creates or reconciles exactly the commit described by the durable
// staged-tree intent. A crash after commit can therefore recover the commit
// identity without trusting arbitrary branch topology.
func (manager *PublicationManager) Commit(ctx context.Context, request PublicationRequest) (string, error) {
	if err := validatePublicationRequest(request); err != nil {
		return "", err
	}
	if err := manager.validate(); err != nil {
		return "", err
	}
	if request.ExpectedTreeSHA == "" {
		return "", errors.New("controller commit requires durable staged-tree intent")
	}
	branch, err := manager.git.output(ctx, request.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve controller commit branch: %w", err)
	}
	if branch != request.Branch {
		return "", fmt.Errorf("worktree is on branch %q, expected recorded branch %q", branch, request.Branch)
	}
	head, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve publication HEAD: %w", err)
	}
	if head == request.BaseSHA {
		if request.ExpectedCommitSHA != "" {
			return "", errors.New("recorded controller commit is missing from the workflow branch")
		}
		// Re-stage immediately before committing. On a retry, this proves that
		// the files just checked still produce the durable tree intent instead
		// of silently committing an older index snapshot.
		if err := manager.git.command(ctx, request.Worktree, "add", "--all", "--", "."); err != nil {
			return "", fmt.Errorf("restage publication changes: %w", err)
		}
		stagedTree, err := manager.git.output(ctx, request.Worktree, "write-tree")
		if err != nil {
			return "", fmt.Errorf("verify staged publication tree: %w", err)
		}
		if stagedTree != request.ExpectedTreeSHA {
			return "", errors.New("staged publication tree does not match durable commit intent")
		}
		if changed, err := manager.diffChanged(ctx, request.Worktree, "--cached", request.BaseSHA, "--"); err != nil {
			return "", fmt.Errorf("verify staged publication diff: %w", err)
		} else if !changed {
			return "", errors.New("publication requires at least one change from the pinned base")
		}
		if changed, err := manager.diffChanged(ctx, request.Worktree, "--cached", request.BaseSHA, "--", request.SpecificationPath); err != nil {
			return "", fmt.Errorf("verify staged specification: %w", err)
		} else if !changed {
			return "", errors.New("publication diff must include the generated specification")
		}
		if err := manager.git.command(ctx, request.Worktree, "commit", "--no-gpg-sign", "--no-verify", "-m", request.CommitMessage); err != nil {
			return "", fmt.Errorf("create controller-owned commit: %w", err)
		}
		commitSHA, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve controller-owned commit: %w", err)
		}
		return commitSHA, nil
	}

	if err := manager.git.command(ctx, request.Worktree, "merge-base", "--is-ancestor", request.BaseSHA, "HEAD"); err != nil {
		return "", fmt.Errorf("verify pinned base ancestry: %w", err)
	}
	countText, err := manager.git.output(ctx, request.Worktree, "rev-list", "--count", request.BaseSHA+"..HEAD")
	if err != nil {
		return "", fmt.Errorf("count publication commits: %w", err)
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count != 1 {
		return "", fmt.Errorf("recorded branch must contain exactly one controller-owned commit above the pinned base, got %q", countText)
	}
	commitTree, err := manager.git.output(ctx, request.Worktree, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve committed publication tree: %w", err)
	}
	if commitTree != request.ExpectedTreeSHA {
		return "", errors.New("existing commit tree does not match durable controller intent")
	}
	message, err := manager.git.output(ctx, request.Worktree, "log", "-1", "--format=%B")
	if err != nil {
		return "", fmt.Errorf("read existing publication commit message: %w", err)
	}
	if message != request.CommitMessage {
		return "", errors.New("existing commit message does not match durable controller intent")
	}
	if request.ExpectedCommitSHA != "" && head != request.ExpectedCommitSHA {
		return "", errors.New("workflow branch does not contain the recorded controller commit")
	}
	status, err := manager.git.output(ctx, request.Worktree, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("verify committed worktree state: %w", err)
	}
	if status != "" {
		return "", errors.New("worktree changed after the controller-owned commit")
	}
	return head, nil
}

// Push publishes only the fully-qualified recorded local branch to the same
// branch name on origin.
func (manager *PublicationManager) Push(ctx context.Context, worktree, branch string) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree || !validRefComponentPath(branch) {
		return errors.New("Git push target is malformed")
	}
	ref := "refs/heads/" + branch
	if err := manager.git.command(ctx, worktree, "push", "--porcelain", "origin", ref+":"+ref); err != nil {
		return fmt.Errorf("push recorded branch %q: %w", branch, err)
	}
	return nil
}

func (manager *PublicationManager) validate() error {
	if manager == nil || manager.git == nil || manager.git.runner == nil || strings.TrimSpace(manager.git.binary) == "" {
		return errors.New("Git publication manager is not configured")
	}
	return nil
}

func (manager *PublicationManager) diffChanged(ctx context.Context, worktree string, arguments ...string) (bool, error) {
	result, err := manager.git.run(ctx, worktree, append([]string{"diff", "--quiet"}, arguments...)...)
	if err == nil {
		return false, nil
	}
	if isExitCode(err, 1) {
		return true, nil
	}
	return false, commandError(result, err)
}

func validatePublicationRequest(request PublicationRequest) error {
	if !filepath.IsAbs(request.Worktree) || filepath.Clean(request.Worktree) != request.Worktree {
		return errors.New("publication worktree must be an absolute clean path")
	}
	if !validRefComponentPath(request.Branch) || !isFullObjectID(request.BaseSHA) {
		return errors.New("publication branch or pinned base is malformed")
	}
	if request.ExpectedCommitSHA != "" && !isFullObjectID(request.ExpectedCommitSHA) {
		return errors.New("expected controller commit SHA is malformed")
	}
	if request.ExpectedTreeSHA != "" && !isFullObjectID(request.ExpectedTreeSHA) {
		return errors.New("expected publication tree SHA is malformed")
	}
	if request.SpecificationPath == "" || path.Clean(request.SpecificationPath) != request.SpecificationPath || path.IsAbs(request.SpecificationPath) || request.SpecificationPath == ".." || strings.HasPrefix(request.SpecificationPath, "../") || strings.ContainsAny(request.SpecificationPath, "\\\x00") {
		return errors.New("publication specification path is malformed")
	}
	if strings.TrimSpace(request.CommitMessage) == "" || strings.ContainsAny(request.CommitMessage, "\r\n\x00") {
		return errors.New("publication commit message must be one non-empty line")
	}
	return nil
}

func isExitCode(err error, code int) bool {
	var exitError *processrun.ExitError
	return errors.As(err, &exitError) && exitError.Result.ExitCode == code
}
