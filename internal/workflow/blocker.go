package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/state"
)

var (
	knownCredentialPattern = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-(?:proj-)?[A-Za-z0-9_-]{20,}|(?:sk|rk)_live_[A-Za-z0-9]{20,}|AKIA[A-Z0-9]{16}|AIza[A-Za-z0-9_-]{30,}|xox[baprs]-[A-Za-z0-9-]{10,})\b`)
	credentialAssignment   = regexp.MustCompile(`(?i)\b((?:[A-Za-z0-9]+[_-])*(?:token|secret|password|passwd|api[_-]?key|access[_-]?key)(?:[_-][A-Za-z0-9]+)*)(\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
)

// AuthenticatedActorResolver identifies the principal selected by GitHub
// authentication.
type AuthenticatedActorResolver interface {
	AuthenticatedActor(context.Context, string) (githubapi.Actor, error)
}

// IssueCommentReader retrieves issue comments for blocker reconciliation and
// resume selection.
type IssueCommentReader interface {
	ListIssueComments(context.Context, string, string, int) ([]githubapi.IssueComment, error)
}

// IssueCommentWriter publishes blocker comments.
type IssueCommentWriter interface {
	PostIssueComment(context.Context, string, string, int, string) error
}

type blockerCommentGateway interface {
	AuthenticatedActorResolver
	IssueCommentReader
	IssueCommentWriter
}

// BlockerPublisher durably publishes one typed agent question.
type BlockerPublisher interface {
	Publish(context.Context, string, string, BlockerRequest) (state.Manifest, error)
}

// BlockerService implements the blocker outbox: intent is durable before the
// GitHub write and the workflow blocks only after stable comment identity is
// durable.
type BlockerService struct {
	reader     SpecificationManifestReader
	transition SpecificationTransitioner
	comments   blockerCommentGateway
	now        func() time.Time
}

// NewBlockerService constructs blocker publication at the application-service
// seam.
func NewBlockerService(reader SpecificationManifestReader, transition SpecificationTransitioner, comments blockerCommentGateway, now func() time.Time) *BlockerService {
	return &BlockerService{reader: reader, transition: transition, comments: comments, now: now}
}

// Publish persists or reuses an unanswered blocker intent, reconciles its
// uniquely marked issue comment, and finally records blocked state.
func (service *BlockerService) Publish(ctx context.Context, controllerRoot, workflowID string, request BlockerRequest) (state.Manifest, error) {
	if service == nil || service.reader == nil || service.transition == nil || service.comments == nil || service.now == nil {
		return state.Manifest{}, errors.New("blocker service is not fully configured")
	}
	if request.Phase != state.PhaseSpec && request.Phase != state.PhaseImplementation && request.Phase != state.PhaseReview {
		return state.Manifest{}, errors.New("blocker phase is not resumable")
	}
	question := redactBlockerQuestion(request.Question)
	if question == "" {
		return state.Manifest{}, errors.New("blocker question is empty after redaction")
	}

	current, err := service.reader.Read(controllerRoot, workflowID)
	if err != nil {
		return state.Manifest{}, fmt.Errorf("read workflow for blocker publication: %w", err)
	}
	if current.Phase != request.Phase {
		return current, fmt.Errorf("blocker phase %s does not match workflow phase %s", request.Phase, current.Phase)
	}
	if current.Status == state.StatusBlocked {
		if current.Blocker != nil && current.Blocker.Question == question {
			return current, nil
		}
		return current, errors.New("workflow is already blocked by a different question")
	}
	if current.Status != state.StatusRunning {
		return current, fmt.Errorf("blocker publication requires running workflow, got %s/%s", current.Phase, current.Status)
	}

	if current.Blocker == nil || current.Blocker.Answer != nil {
		publisher, actorErr := service.comments.AuthenticatedActor(ctx, controllerRoot)
		if actorErr != nil {
			return service.failRunning(controllerRoot, current, fmt.Errorf("resolve blocker publisher: %w", actorErr))
		}
		if strings.TrimSpace(publisher.Login) == "" {
			return service.failRunning(controllerRoot, current, errors.New("authenticated GitHub publisher is missing"))
		}
		sequence := current.BlockerSequence + 1
		blockerID := state.BlockerID(sequence)
		intent := current
		intent.BlockerSequence = sequence
		intent.Blocker = &state.Blocker{
			ID: blockerID, Phase: request.Phase, Question: question, Actor: publisher.Login,
			Marker: state.BlockerMarker(current.WorkflowID, blockerID), CreatedAt: service.now().UTC(),
		}
		if err := service.transition.Transition(controllerRoot, workflowID, intent); err != nil {
			return current, fmt.Errorf("persist blocker intent: %w", err)
		}
		current = intent
	} else if current.Blocker.Question != question {
		return current, errors.New("an unanswered blocker intent already exists with a different question")
	}

	if current.Blocker.Comment == nil {
		comment, reconciled, reconcileErr := service.reconcile(ctx, controllerRoot, current)
		current = reconciled
		if reconcileErr != nil {
			return service.failRunning(controllerRoot, current, reconcileErr)
		}
		withComment := current
		withComment.Blocker = cloneBlocker(current.Blocker)
		withComment.Blocker.Comment = &state.SourceReference{ID: comment.ID, URL: comment.URL}
		if err := service.transition.Transition(controllerRoot, workflowID, withComment); err != nil {
			// A durable write failure must leave the intent retryable. In
			// particular, do not replace it with failed state after the external
			// post may already have succeeded.
			return current, fmt.Errorf("persist blocker comment identity: %w", err)
		}
		current = withComment
	}

	blocked := current
	blocked.Status = state.StatusBlocked
	if err := service.transition.Transition(controllerRoot, workflowID, blocked); err != nil {
		return current, fmt.Errorf("persist blocked workflow state: %w", err)
	}
	return blocked, nil
}

func (service *BlockerService) reconcile(ctx context.Context, controllerRoot string, manifest state.Manifest) (githubapi.IssueComment, state.Manifest, error) {
	comments, err := service.comments.ListIssueComments(ctx, controllerRoot, manifest.Repository, manifest.Issue.Number)
	if err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	matched, err := markedComment(comments, manifest, manifest.Blocker.Marker)
	if err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	if matched.ID != "" {
		updated, updateErr := service.persistBlockerPublisher(controllerRoot, manifest, matched.Author.Login)
		return matched, updated, updateErr
	}
	publisher, err := service.comments.AuthenticatedActor(ctx, controllerRoot)
	if err != nil {
		return githubapi.IssueComment{}, manifest, fmt.Errorf("resolve blocker publisher before posting: %w", err)
	}
	manifest, err = service.persistBlockerPublisher(controllerRoot, manifest, publisher.Login)
	if err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	body := blockerCommentBody(manifest.Blocker)
	if err := service.comments.PostIssueComment(ctx, controllerRoot, manifest.Repository, manifest.Issue.Number, body); err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	comments, err = service.comments.ListIssueComments(ctx, controllerRoot, manifest.Repository, manifest.Issue.Number)
	if err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	matched, err = markedComment(comments, manifest, manifest.Blocker.Marker)
	if err != nil {
		return githubapi.IssueComment{}, manifest, err
	}
	if matched.ID == "" {
		return githubapi.IssueComment{}, manifest, errors.New("posted blocker comment could not be reconciled")
	}
	if !strings.EqualFold(matched.Author.Login, manifest.Blocker.Actor) {
		return githubapi.IssueComment{}, manifest, errors.New("posted blocker comment author does not match the persisted publisher")
	}
	return matched, manifest, nil
}

func markedComment(comments []githubapi.IssueComment, manifest state.Manifest, marker string) (githubapi.IssueComment, error) {
	var matched githubapi.IssueComment
	for _, comment := range comments {
		if !hasCanonicalMarker(comment.Body, marker) {
			continue
		}
		if !validIssueComment(comment, manifest.Repository, manifest.Issue.Number) {
			continue
		}
		if matched.ID != "" {
			return githubapi.IssueComment{}, errors.New("multiple GitHub comments contain the blocker marker")
		}
		matched = comment
	}
	return matched, nil
}

func (service *BlockerService) persistBlockerPublisher(controllerRoot string, manifest state.Manifest, login string) (state.Manifest, error) {
	if strings.TrimSpace(login) == "" {
		return manifest, errors.New("authenticated GitHub publisher is missing")
	}
	if strings.EqualFold(manifest.Blocker.Actor, login) {
		return manifest, nil
	}
	updated := manifest
	updated.Blocker = cloneBlocker(manifest.Blocker)
	updated.Blocker.Actor = login
	if err := service.transition.Transition(controllerRoot, manifest.WorkflowID, updated); err != nil {
		return manifest, fmt.Errorf("persist current blocker publisher: %w", err)
	}
	return updated, nil
}

func blockerCommentBody(blocker *state.Blocker) string {
	return fmt.Sprintf("AWDev needs human input to continue the `%s` phase:\n\n%s\n\n%s", blocker.Phase, blocker.Question, blocker.Marker)
}

func hasCanonicalMarker(body, marker string) bool {
	trimmed := strings.TrimRight(body, "\r\n")
	lastNewline := strings.LastIndexByte(trimmed, '\n')
	lastLine := strings.TrimSuffix(trimmed[lastNewline+1:], "\r")
	return lastLine == marker
}

func redactBlockerQuestion(question string) string {
	redacted := knownCredentialPattern.ReplaceAllString(question, "[REDACTED]")
	redacted = credentialAssignment.ReplaceAllString(redacted, "$1$2[REDACTED]")
	return strings.TrimSpace(redacted)
}

func validIssueComment(comment githubapi.IssueComment, repository string, issueNumber int) bool {
	if strings.TrimSpace(comment.ID) == "" || strings.TrimSpace(comment.Body) == "" || strings.TrimSpace(comment.Author.Login) == "" || strings.TrimSpace(string(comment.Author.Type)) == "" || comment.CreatedAt.IsZero() {
		return false
	}
	if _, err := strconv.ParseUint(comment.ID, 10, 64); err != nil {
		return false
	}
	parsed, err := url.Parse(comment.URL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" {
		return false
	}
	wantPath := fmt.Sprintf("/%s/issues/%d", repository, issueNumber)
	return parsed.Path == wantPath && parsed.Fragment == "issuecomment-"+comment.ID
}

func cloneBlocker(blocker *state.Blocker) *state.Blocker {
	if blocker == nil {
		return nil
	}
	cloned := *blocker
	if blocker.Comment != nil {
		comment := *blocker.Comment
		cloned.Comment = &comment
	}
	if blocker.Answer != nil {
		answer := *blocker.Answer
		cloned.Answer = &answer
	}
	return &cloned
}

func (service *BlockerService) failRunning(controllerRoot string, current state.Manifest, cause error) (state.Manifest, error) {
	failed := current
	failed.Status = state.StatusFailed
	failed.LastError = &state.WorkflowError{Code: "technical_failure", Message: sanitizeTechnicalError(cause, controllerRoot)}
	if err := service.transition.Transition(controllerRoot, current.WorkflowID, failed); err != nil {
		return current, errors.Join(cause, fmt.Errorf("persist blocker publication failure: %w", err))
	}
	return failed, cause
}
