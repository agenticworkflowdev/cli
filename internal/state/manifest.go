package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CurrentSchemaVersion is the manifest format written by this version.
const CurrentSchemaVersion = 1

// Source names the external system that owns the workflow item.
type Source string

const (
	// SourceGitHub identifies a GitHub issue workflow.
	SourceGitHub Source = "github"
)

// Phase names one durable workflow phase.
type Phase string

const (
	PhaseInit           Phase = "init"
	PhaseSpec           Phase = "spec"
	PhaseImplementation Phase = "implementation"
	PhaseReview         Phase = "review"
	PhasePullRequest    Phase = "pull_request"
	PhaseDone           Phase = "done"
)

// Status is the durable workflow execution condition.
type Status string

const (
	StatusRunning Status = "running"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
	StatusDone    Status = "done"
)

// IssueSnapshot is the immutable GitHub issue input used by every later phase.
type IssueSnapshot struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReviewCounters record bounded independent review progress.
type ReviewCounters struct {
	Attempt     int `json:"attempt"`
	MaxAttempts int `json:"max_attempts"`
}

// Blocker records a durable request for human judgment. A blocker may be
// present while running as publication intent and is required when blocked.
type Blocker struct {
	ID        string           `json:"id"`
	Phase     Phase            `json:"phase"`
	Question  string           `json:"question"`
	Actor     string           `json:"actor"`
	Marker    string           `json:"marker"`
	CreatedAt time.Time        `json:"created_at"`
	Comment   *SourceReference `json:"comment,omitempty"`
	Answer    *BlockerAnswer   `json:"answer,omitempty"`
}

// SourceReference identifies one durable GitHub comment.
type SourceReference struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// BlockerAnswer records the human input selected when resuming a blocker.
type BlockerAnswer struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// PullRequest is the stable external result of a completed workflow.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// WorkflowError is a sanitized, durable technical failure.
type WorkflowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Manifest is the complete durable controller state for one issue workflow.
type Manifest struct {
	SchemaVersion     int             `json:"schema_version"`
	WorkflowID        string          `json:"workflow_id"`
	Source            Source          `json:"source"`
	Repository        string          `json:"repository"`
	DefaultBranch     string          `json:"default_branch"`
	Issue             IssueSnapshot   `json:"issue"`
	Actor             string          `json:"actor"`
	Phase             Phase           `json:"phase"`
	Status            Status          `json:"status"`
	Branch            string          `json:"branch"`
	BaseSHA           string          `json:"base_sha"`
	Worktree          string          `json:"worktree"`
	SpecificationPath string          `json:"specification_path,omitempty"`
	Review            *ReviewCounters `json:"review,omitempty"`
	BlockerSequence   int             `json:"blocker_sequence,omitempty"`
	Blocker           *Blocker        `json:"blocker,omitempty"`
	LastError         *WorkflowError  `json:"last_error,omitempty"`
	CommitTreeSHA     string          `json:"commit_tree_sha,omitempty"`
	CommitSHA         string          `json:"commit_sha,omitempty"`
	PullRequest       *PullRequest    `json:"pull_request,omitempty"`
}

// ManifestWriter persists a complete validated workflow manifest.
type ManifestWriter interface {
	Save(string, Manifest) error
}

// NewWorkflowID returns an opaque, filesystem-safe random workflow identity.
func NewWorkflowID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate workflow identity: %w", err)
	}
	return "wf_" + hex.EncodeToString(random), nil
}

// GitHubIssueKey returns the deterministic coordination key for an issue.
func GitHubIssueKey(issueNumber int) (string, error) {
	if issueNumber <= 0 {
		return "", errors.New("issue number must be positive")
	}
	return "gh-" + strconv.Itoa(issueNumber), nil
}

// Validate rejects state that later workflow phases cannot safely consume.
func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", manifest.SchemaVersion)
	}
	if err := validateWorkflowID(manifest.WorkflowID); err != nil {
		return err
	}
	if manifest.Source != SourceGitHub {
		return fmt.Errorf("invalid workflow source %q", manifest.Source)
	}
	if !validRepository(manifest.Repository) {
		return errors.New("GitHub repository identity is missing or malformed")
	}
	if !validGitRef(manifest.DefaultBranch) {
		return errors.New("default branch is missing or malformed")
	}
	if manifest.Issue.Number <= 0 {
		return errors.New("issue number must be positive")
	}
	if invalidSingleLine(manifest.Issue.Title) {
		return errors.New("issue title is missing or malformed")
	}
	if err := validateIssueURL(manifest.Repository, manifest.Issue); err != nil {
		return err
	}
	if manifest.Issue.State != "OPEN" {
		return errors.New("saved issue state must be OPEN")
	}
	if manifest.Issue.UpdatedAt.IsZero() {
		return errors.New("issue updated_at is required")
	}
	if invalidSingleLine(manifest.Actor) {
		return errors.New("authenticated actor is missing or malformed")
	}
	if !validGitRef(manifest.Branch) {
		return errors.New("workflow branch is missing or malformed")
	}
	if !validObjectID(manifest.BaseSHA) {
		return errors.New("base SHA must be a full lowercase Git object ID")
	}
	if filepath.IsAbs(manifest.Worktree) || path.IsAbs(manifest.Worktree) || path.Clean(manifest.Worktree) != manifest.Worktree || strings.ContainsAny(manifest.Worktree, "\\\x00") {
		return errors.New("worktree must be a clean repository-relative path")
	}
	worktreeParent := path.Dir(manifest.Worktree)
	issueKey, _ := GitHubIssueKey(manifest.Issue.Number)
	worktreeName := path.Base(manifest.Worktree)
	if worktreeParent != path.Join(".awdev", "worktrees") || !strings.HasPrefix(worktreeName, issueKey+"-") {
		return errors.New("worktree must be an issue-derived path under .awdev/worktrees")
	}
	if manifest.Branch != worktreeName {
		return errors.New("workflow branch must match the issue-derived worktree name")
	}
	wantSpecificationPath, _ := SpecificationPathForBranch(manifest.Branch)
	legacySpecificationPath := path.Join(".awdev", "specs", manifest.WorkflowID+".md")
	if manifest.SpecificationPath != "" && manifest.SpecificationPath != wantSpecificationPath && manifest.SpecificationPath != legacySpecificationPath {
		return fmt.Errorf("specification path must be %q", wantSpecificationPath)
	}
	if manifest.Review != nil {
		if manifest.Review.MaxAttempts < 1 || manifest.Review.Attempt < 0 || manifest.Review.Attempt > manifest.Review.MaxAttempts {
			return errors.New("review counters are invalid")
		}
	}
	if (manifest.Phase == PhaseReview || manifest.Phase == PhasePullRequest || manifest.Phase == PhaseDone) && manifest.Review == nil {
		return errors.New("review, pull_request, and done phases require review counters")
	}
	if err := validatePhaseStatus(manifest.Phase, manifest.Status); err != nil {
		return err
	}
	if manifest.BlockerSequence < 0 {
		return errors.New("blocker sequence cannot be negative")
	}
	if manifest.Blocker != nil {
		wantID := BlockerID(manifest.BlockerSequence)
		wantMarker := BlockerMarker(manifest.WorkflowID, wantID)
		if manifest.BlockerSequence < 1 || manifest.Blocker.ID != wantID || strings.TrimSpace(manifest.Blocker.Question) == "" || manifest.Blocker.Phase != manifest.Phase ||
			invalidSingleLine(manifest.Blocker.Actor) || manifest.Blocker.Marker != wantMarker || manifest.Blocker.CreatedAt.IsZero() {
			return errors.New("blocker identity, sequence, phase, question, publisher, marker, and timestamp are required and must match the current workflow")
		}
		if manifest.Blocker.Comment != nil && !validSourceReference(*manifest.Blocker.Comment) {
			return errors.New("blocker comment identity and URL are invalid")
		}
		if manifest.Blocker.Answer != nil {
			answer := manifest.Blocker.Answer
			if manifest.Blocker.Comment == nil || !validSourceReference(SourceReference{ID: answer.ID, URL: answer.URL}) || strings.TrimSpace(answer.Body) == "" || invalidSingleLine(answer.Author) || answer.CreatedAt.IsZero() {
				return errors.New("blocker answer identity, URL, body, author, and timestamp are required")
			}
		}
	}
	if manifest.Status == StatusBlocked && (manifest.Blocker == nil || manifest.Blocker.Comment == nil) {
		return errors.New("blocked workflow requires a blocker with a published comment")
	}
	if manifest.LastError != nil && (invalidSingleLine(manifest.LastError.Code) || strings.TrimSpace(manifest.LastError.Message) == "") {
		return errors.New("last_error code and message are required")
	}
	if manifest.Status == StatusFailed && manifest.LastError == nil {
		return errors.New("failed workflow requires last_error")
	}
	if manifest.Status != StatusFailed && manifest.LastError != nil {
		return errors.New("last_error is only valid for a failed workflow")
	}
	if manifest.PullRequest != nil {
		if manifest.PullRequest.Number <= 0 {
			return errors.New("pull request number must be positive")
		}
		want := "https://github.com/" + manifest.Repository + "/pull/" + strconv.Itoa(manifest.PullRequest.Number)
		if manifest.PullRequest.URL != want {
			return errors.New("pull request URL does not match the manifest repository and number")
		}
	}
	if manifest.CommitTreeSHA != "" {
		if !validObjectID(manifest.CommitTreeSHA) {
			return errors.New("commit tree SHA must be a full lowercase Git object ID")
		}
		if manifest.Phase != PhasePullRequest && manifest.Phase != PhaseDone {
			return errors.New("commit tree SHA is only valid during pull_request or done")
		}
	}
	if manifest.CommitSHA != "" {
		if !validObjectID(manifest.CommitSHA) {
			return errors.New("commit SHA must be a full lowercase Git object ID")
		}
		if manifest.Phase != PhasePullRequest && manifest.Phase != PhaseDone {
			return errors.New("commit SHA is only valid during pull_request or done")
		}
		if manifest.CommitTreeSHA == "" {
			return errors.New("commit SHA requires a recorded commit tree SHA")
		}
	}
	if manifest.Status == StatusDone && manifest.PullRequest == nil {
		return errors.New("done workflow requires a pull request")
	}
	if manifest.PullRequest != nil && manifest.Phase != PhasePullRequest && manifest.Phase != PhaseDone {
		return errors.New("pull request is only valid during pull_request or done")
	}
	return nil
}

// BlockerID returns the monotonically increasing identity for one workflow's
// blocker sequence.
func BlockerID(sequence int) string {
	return "blocker-" + strconv.Itoa(sequence)
}

// BlockerMarker returns the exact hidden marker used to reconcile a published
// blocker comment after interruption.
func BlockerMarker(workflowID, blockerID string) string {
	return "<!-- awdev:blocker workflow=" + workflowID + " id=" + blockerID + " -->"
}

// ResolveWorktreePath converts a validated manifest worktree path into the
// absolute path required by filesystem and child-process boundaries.
func ResolveWorktreePath(controllerRoot, relativePath string) (string, error) {
	if !filepath.IsAbs(controllerRoot) || filepath.Clean(controllerRoot) != controllerRoot {
		return "", errors.New("controller root must be an absolute clean path")
	}
	if filepath.IsAbs(relativePath) || path.IsAbs(relativePath) || path.Clean(relativePath) != relativePath || strings.ContainsAny(relativePath, "\\\x00") {
		return "", errors.New("worktree must be a clean repository-relative path")
	}
	if path.Dir(relativePath) != path.Join(".awdev", "worktrees") {
		return "", errors.New("worktree must be under .awdev/worktrees")
	}
	return filepath.Join(controllerRoot, filepath.FromSlash(relativePath)), nil
}

// SpecificationPathForBranch returns the stable worktree-relative path for a
// workflow branch's implementation specification.
func SpecificationPathForBranch(branch string) (string, error) {
	if !validGitRef(branch) || path.Base(branch) != branch {
		return "", errors.New("workflow branch must be a single safe path component")
	}
	return path.Join(".awdev", "specs", branch+".md"), nil
}

// RelativizeControllerPaths replaces absolute paths beneath the controller's
// .awdev directory with their stable repository-relative representation.
func RelativizeControllerPaths(controllerRoot, value string) string {
	absoluteAWDev := filepath.Join(filepath.Clean(controllerRoot), ".awdev")
	if value == absoluteAWDev {
		return ".awdev"
	}
	value = strings.ReplaceAll(value, absoluteAWDev+string(filepath.Separator), ".awdev"+string(filepath.Separator))
	for _, boundary := range []string{" ", "\t", "\r", "\n", ":", ",", ";", ")", "]", "}", `"`, `'`} {
		value = strings.ReplaceAll(value, absoluteAWDev+boundary, ".awdev"+boundary)
	}
	if strings.HasSuffix(value, absoluteAWDev) {
		value = strings.TrimSuffix(value, absoluteAWDev) + ".awdev"
	}
	return filepath.ToSlash(value)
}

func validatePhaseStatus(phase Phase, status Status) error {
	if !validPhases[phase] {
		return fmt.Errorf("invalid workflow phase %q", phase)
	}
	if !validConditions[workflowCondition{phase: phase, status: status}] {
		return fmt.Errorf("status %q is invalid for phase %q", status, phase)
	}
	return nil
}

func validateWorkflowID(workflowID string) error {
	if len(workflowID) != 35 || !strings.HasPrefix(workflowID, "wf_") {
		return fmt.Errorf("invalid workflow identity %q", workflowID)
	}
	for _, character := range workflowID[3:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return fmt.Errorf("invalid workflow identity %q", workflowID)
		}
	}
	return nil
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validIdentityPart(parts[0]) && validIdentityPart(parts[1])
}

func validIdentityPart(value string) bool {
	if invalidSingleLine(value) || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsFunc(value, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.')
	})
}

func validGitRef(value string) bool {
	if invalidSingleLine(value) || value == "@" || strings.HasPrefix(value, "-") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") ||
		strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return !strings.ContainsFunc(value, func(character rune) bool {
		return !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f'))
	})
}

func validateIssueURL(repository string, issue IssueSnapshot) error {
	parsed, err := url.Parse(issue.URL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("issue URL is missing or malformed")
	}
	wantPath := "/" + repository + "/issues/" + strconv.Itoa(issue.Number)
	if parsed.Path != wantPath {
		return errors.New("issue URL does not match the manifest repository and number")
	}
	return nil
}

func validSourceReference(reference SourceReference) bool {
	if invalidSingleLine(reference.ID) {
		return false
	}
	parsed, err := url.Parse(reference.URL)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "github.com") && parsed.Path != "" && parsed.RawQuery == "" && parsed.Fragment != ""
}

func invalidSingleLine(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return true
	}
	return strings.ContainsFunc(value, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	})
}
