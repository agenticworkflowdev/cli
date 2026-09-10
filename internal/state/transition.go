package state

import (
	"errors"
	"fmt"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

// ManifestRepository is the durable boundary used by transitions.
type ManifestRepository interface {
	Read(string, string) (Manifest, error)
	Save(string, Manifest) error
}

// TransitionService validates and persists one complete state transition.
type TransitionService struct {
	repository ManifestRepository
}

// NewTransitionService constructs the single policy gate for phase changes.
func NewTransitionService(repository ManifestRepository) *TransitionService {
	return &TransitionService{repository: repository}
}

// Transition rejects an invalid edge before replacing durable state.
func (service *TransitionService) Transition(controllerRoot, workflowID string, next Manifest) error {
	if service == nil || service.repository == nil {
		return errors.New("transition service is not configured")
	}
	if next.WorkflowID != workflowID {
		return errors.New("transition workflow identity does not match the requested workflow")
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("validate next workflow state: %w", err)
	}
	current, err := service.repository.Read(controllerRoot, workflowID)
	if err != nil {
		return fmt.Errorf("read current workflow state: %w", err)
	}
	if !current.hasSameBootstrap(next) {
		return errors.New("transition cannot change immutable workflow bootstrap data")
	}
	if !allowedManifestTransition(current, next) {
		return fmt.Errorf("invalid workflow transition %s/%s -> %s/%s", current.Phase, current.Status, next.Phase, next.Status)
	}
	if err := validateTransitionData(current, next); err != nil {
		return fmt.Errorf("invalid workflow transition data: %w", err)
	}
	if err := service.repository.Save(controllerRoot, next); err != nil {
		return fmt.Errorf("persist workflow transition: %w", err)
	}
	return nil
}

func allowedManifestTransition(current, next Manifest) bool {
	if !allowedTransition(current.Phase, current.Status, next.Phase, next.Status) {
		return false
	}
	if current.Phase == PhaseRecon && next.Phase == PhaseSpec {
		return !current.SkipSpecification && !next.SkipSpecification
	}
	if current.Phase == PhaseRecon && next.Phase == PhaseImplementation {
		return current.SkipSpecification && next.SkipSpecification
	}
	return true
}

func validateTransitionData(current, next Manifest) error {
	if err := validateAgentSessionTransition(current, next); err != nil {
		return err
	}
	if next.BlockerSequence < current.BlockerSequence || next.BlockerSequence > current.BlockerSequence+1 {
		return errors.New("blocker sequence must be monotonic and advance one blocker at a time")
	}
	if next.BlockerSequence == current.BlockerSequence+1 {
		if next.Blocker == nil || current.Blocker != nil && current.Blocker.Answer == nil {
			return errors.New("blocker sequence can advance only when recording a new blocker intent")
		}
	} else if current.Blocker == nil && next.Blocker != nil {
		return errors.New("new blocker intent must advance the blocker sequence")
	}
	if current.Phase != next.Phase && next.Blocker != nil {
		return errors.New("a phase transition must clear the previous blocker")
	}
	if next.Status == StatusBlocked {
		if current.Blocker == nil || !sameBlockerIntent(current.Blocker, next.Blocker) {
			return errors.New("running workflow must persist blocker intent before becoming blocked")
		}
	}
	if current.Blocker == nil && next.Blocker != nil && (next.Blocker.Comment != nil || next.Blocker.Answer != nil) {
		return errors.New("new blocker intent must be persisted before its external comment identity")
	}
	if current.Status == StatusBlocked && next.Status == StatusRunning {
		if !sameBlockerIntent(current.Blocker, next.Blocker) || next.Blocker.Answer == nil {
			return errors.New("blocked workflow must preserve its blocker and answer before resuming")
		}
	}
	if current.Phase == next.Phase && current.Blocker != nil && current.Blocker.Answer == nil && !sameBlockerIntent(current.Blocker, next.Blocker) {
		if current.Status != next.Status || !pendingBlockerPublisherUpdate(current.Blocker, next.Blocker) {
			return errors.New("unanswered blocker intent cannot be replaced")
		}
	}
	if sameBlockerIntent(current.Blocker, next.Blocker) {
		if current.Blocker.Comment != nil && !sameSourceReference(current.Blocker.Comment, next.Blocker.Comment) {
			return errors.New("published blocker comment identity cannot change")
		}
		if current.Blocker.Answer != nil && !sameBlockerAnswer(current.Blocker.Answer, next.Blocker.Answer) {
			return errors.New("selected blocker answer cannot change")
		}
	}
	if current.PullRequest != nil && !samePullRequest(current.PullRequest, next.PullRequest) {
		return errors.New("recorded pull request identity cannot change")
	}
	if current.PullRequest == nil && next.PullRequest != nil && next.CommitSHA == "" {
		return errors.New("controller commit identity must be persisted before pull request identity")
	}
	if current.CommitTreeSHA != "" && current.CommitTreeSHA != next.CommitTreeSHA {
		return errors.New("recorded controller commit tree cannot change")
	}
	if current.CommitTreeSHA == "" && next.CommitTreeSHA != "" && !(current.Phase == PhasePullRequest && current.Status == StatusRunning && next.Phase == PhasePullRequest && next.Status == StatusRunning) {
		return errors.New("controller commit tree can be introduced only during pull_request/running")
	}
	if current.CommitSHA != "" && current.CommitSHA != next.CommitSHA {
		return errors.New("recorded controller commit identity cannot change")
	}
	if current.CommitSHA == "" && next.CommitSHA != "" && !(current.Phase == PhasePullRequest && current.Status == StatusRunning && next.Phase == PhasePullRequest && next.Status == StatusRunning) {
		return errors.New("controller commit identity can be introduced only during pull_request/running")
	}
	if current.CommitSHA == "" && next.CommitSHA != "" && current.CommitTreeSHA == "" {
		return errors.New("controller commit tree must be persisted before controller commit identity")
	}
	if current.Phase == PhaseReview && next.Phase == PhasePullRequest && (next.CommitTreeSHA != "" || next.CommitSHA != "") {
		return errors.New("pull_request phase must be persisted before its controller commit intent")
	}
	if next.Phase == PhaseDone {
		if current.PullRequest == nil || !samePullRequest(current.PullRequest, next.PullRequest) {
			return errors.New("pull request identity must be persisted before completion")
		}
	}
	if current.Phase == PhaseReview && next.Phase == PhasePullRequest && next.PullRequest != nil {
		return errors.New("pull_request phase must be persisted before its external identity")
	}
	if err := validateReviewTransition(current, next); err != nil {
		return err
	}
	return nil
}

func validateAgentSessionTransition(current, next Manifest) error {
	currentProvider, currentSpecification, currentImplementation := agentSessionValues(current.AgentSessions)
	nextProvider, nextSpecification, nextImplementation := agentSessionValues(next.AgentSessions)
	if currentProvider != "" && currentProvider != nextProvider {
		return errors.New("recorded agent session provider cannot change")
	}
	if currentSpecification != "" && currentSpecification != nextSpecification {
		return errors.New("recorded specification agent session cannot change")
	}
	if currentImplementation != "" && currentImplementation != nextImplementation {
		return errors.New("recorded implementation agent session cannot change")
	}
	if currentSpecification == "" && nextSpecification != "" && !(current.Phase == PhaseSpec && next.Phase == PhaseSpec && current.Status == StatusRunning && next.Status == StatusRunning) {
		return errors.New("specification agent session can be introduced only during spec/running")
	}
	implementationPhase := current.Phase == PhaseImplementation || current.Phase == PhaseReview
	if currentImplementation == "" && nextImplementation != "" && !(implementationPhase && current.Phase == next.Phase && current.Status == StatusRunning && next.Status == StatusRunning) {
		return errors.New("implementation agent session can be introduced only during implementation or review running state")
	}
	return nil
}

func agentSessionValues(sessions *AgentSessions) (agent.Provider, string, string) {
	if sessions == nil {
		return "", "", ""
	}
	return sessions.Provider, sessions.Specification, sessions.Implementation
}

func validateReviewTransition(current, next Manifest) error {
	if current.Review == nil && next.Review != nil && !(current.Phase == PhaseImplementation && next.Phase == PhaseReview) {
		return errors.New("review counters cannot be introduced outside the first review transition")
	}
	if current.Phase == PhaseReview && next.Phase == PhaseImplementation {
		if current.Review == nil || next.Review == nil || next.Review.MaxAttempts != current.Review.MaxAttempts || next.Review.Attempt != current.Review.Attempt+1 {
			return errors.New("review correction must advance the review attempt")
		}
		return nil
	}
	if current.Phase == PhaseImplementation && next.Phase == PhaseReview {
		if next.Review == nil {
			return errors.New("review phase requires review counters")
		}
		if current.Review == nil && next.Review.Attempt != 1 {
			return errors.New("first review must start at attempt 1")
		}
		if current.Review != nil && !sameReviewCounters(current.Review, next.Review) {
			return errors.New("review retry must preserve the advanced review attempt")
		}
		return nil
	}
	if current.Review != nil && !sameReviewCounters(current.Review, next.Review) {
		return errors.New("review state must be preserved until publication")
	}
	if (next.Phase == PhaseReview || next.Phase == PhasePullRequest || next.Phase == PhaseDone) && next.Review == nil {
		return errors.New("review state is required before publication")
	}
	return nil
}

func sameBlockerIntent(left, right *Blocker) bool {
	return left != nil && right != nil && left.ID == right.ID && left.Phase == right.Phase && left.Question == right.Question &&
		left.Actor == right.Actor && left.Marker == right.Marker && left.CreatedAt.Equal(right.CreatedAt)
}

func pendingBlockerPublisherUpdate(current, next *Blocker) bool {
	return current != nil && next != nil && current.ID == next.ID && current.Phase == next.Phase && current.Question == next.Question &&
		current.Marker == next.Marker && current.CreatedAt.Equal(next.CreatedAt) && current.Actor != next.Actor &&
		current.Comment == nil && next.Comment == nil && current.Answer == nil && next.Answer == nil
}

func samePullRequest(left, right *PullRequest) bool {
	return sameNonNil(left, right)
}

func sameSourceReference(left, right *SourceReference) bool {
	return sameNonNil(left, right)
}

func sameBlockerAnswer(left, right *BlockerAnswer) bool {
	return left != nil && right != nil && left.ID == right.ID && left.URL == right.URL && left.Body == right.Body && left.Author == right.Author && left.CreatedAt.Equal(right.CreatedAt)
}

func sameReviewCounters(left, right *ReviewCounters) bool {
	return sameNonNil(left, right)
}

func sameNonNil[T comparable](left, right *T) bool {
	return left != nil && right != nil && *left == *right
}

func (manifest Manifest) hasSameBootstrap(other Manifest) bool {
	return manifest.SchemaVersion == other.SchemaVersion &&
		manifest.WorkflowID == other.WorkflowID &&
		manifest.Source == other.Source &&
		manifest.Repository == other.Repository &&
		manifest.DefaultBranch == other.DefaultBranch &&
		manifest.Issue.Number == other.Issue.Number &&
		manifest.Issue.Title == other.Issue.Title &&
		manifest.Issue.Body == other.Issue.Body &&
		manifest.Issue.URL == other.Issue.URL &&
		manifest.Issue.State == other.Issue.State &&
		manifest.Issue.UpdatedAt.Equal(other.Issue.UpdatedAt) &&
		manifest.Actor == other.Actor &&
		manifest.Branch == other.Branch &&
		manifest.BaseSHA == other.BaseSHA &&
		manifest.Worktree == other.Worktree &&
		manifest.SkipSpecification == other.SkipSpecification &&
		sameSpecificationPath(manifest.SpecificationPath, other.SpecificationPath)
}

func sameSpecificationPath(current, next string) bool {
	return current == next || current == "" && next != ""
}

type workflowCondition struct {
	phase  Phase
	status Status
}

type workflowTransition struct {
	from workflowCondition
	to   workflowCondition
}

var validPhases = map[Phase]bool{
	PhaseInit: true, PhaseRecon: true, PhaseSpec: true, PhaseImplementation: true,
	PhaseReview: true, PhasePullRequest: true, PhaseDone: true,
}

var validConditions = map[workflowCondition]bool{
	{PhaseInit, StatusRunning}:           true,
	{PhaseRecon, StatusRunning}:          true,
	{PhaseRecon, StatusFailed}:           true,
	{PhaseSpec, StatusRunning}:           true,
	{PhaseSpec, StatusBlocked}:           true,
	{PhaseSpec, StatusFailed}:            true,
	{PhaseImplementation, StatusRunning}: true,
	{PhaseImplementation, StatusBlocked}: true,
	{PhaseImplementation, StatusFailed}:  true,
	{PhaseReview, StatusRunning}:         true,
	{PhaseReview, StatusBlocked}:         true,
	{PhaseReview, StatusFailed}:          true,
	{PhasePullRequest, StatusRunning}:    true,
	{PhasePullRequest, StatusFailed}:     true,
	{PhaseDone, StatusDone}:              true,
}

var allowedTransitions = map[workflowTransition]bool{
	transition(PhaseInit, StatusRunning, PhaseInit, StatusRunning):                     true,
	transition(PhaseInit, StatusRunning, PhaseRecon, StatusRunning):                    true,
	transition(PhaseRecon, StatusRunning, PhaseRecon, StatusRunning):                   true,
	transition(PhaseRecon, StatusRunning, PhaseRecon, StatusFailed):                    true,
	transition(PhaseRecon, StatusRunning, PhaseSpec, StatusRunning):                    true,
	transition(PhaseRecon, StatusRunning, PhaseImplementation, StatusRunning):          true,
	transition(PhaseRecon, StatusFailed, PhaseRecon, StatusFailed):                     true,
	transition(PhaseSpec, StatusRunning, PhaseSpec, StatusRunning):                     true,
	transition(PhaseSpec, StatusRunning, PhaseSpec, StatusBlocked):                     true,
	transition(PhaseSpec, StatusRunning, PhaseSpec, StatusFailed):                      true,
	transition(PhaseSpec, StatusRunning, PhaseImplementation, StatusRunning):           true,
	transition(PhaseSpec, StatusBlocked, PhaseSpec, StatusBlocked):                     true,
	transition(PhaseSpec, StatusBlocked, PhaseSpec, StatusRunning):                     true,
	transition(PhaseSpec, StatusBlocked, PhaseSpec, StatusFailed):                      true,
	transition(PhaseSpec, StatusFailed, PhaseSpec, StatusFailed):                       true,
	transition(PhaseImplementation, StatusRunning, PhaseImplementation, StatusRunning): true,
	transition(PhaseImplementation, StatusRunning, PhaseImplementation, StatusBlocked): true,
	transition(PhaseImplementation, StatusRunning, PhaseImplementation, StatusFailed):  true,
	transition(PhaseImplementation, StatusRunning, PhaseReview, StatusRunning):         true,
	transition(PhaseImplementation, StatusBlocked, PhaseImplementation, StatusBlocked): true,
	transition(PhaseImplementation, StatusBlocked, PhaseImplementation, StatusRunning): true,
	transition(PhaseImplementation, StatusBlocked, PhaseImplementation, StatusFailed):  true,
	transition(PhaseImplementation, StatusFailed, PhaseImplementation, StatusFailed):   true,
	transition(PhaseReview, StatusRunning, PhaseReview, StatusRunning):                 true,
	transition(PhaseReview, StatusRunning, PhaseReview, StatusBlocked):                 true,
	transition(PhaseReview, StatusRunning, PhaseReview, StatusFailed):                  true,
	transition(PhaseReview, StatusRunning, PhaseImplementation, StatusRunning):         true,
	transition(PhaseReview, StatusRunning, PhasePullRequest, StatusRunning):            true,
	transition(PhaseReview, StatusBlocked, PhaseReview, StatusBlocked):                 true,
	transition(PhaseReview, StatusBlocked, PhaseReview, StatusRunning):                 true,
	transition(PhaseReview, StatusBlocked, PhaseReview, StatusFailed):                  true,
	transition(PhaseReview, StatusFailed, PhaseReview, StatusFailed):                   true,
	transition(PhasePullRequest, StatusRunning, PhasePullRequest, StatusRunning):       true,
	transition(PhasePullRequest, StatusRunning, PhasePullRequest, StatusFailed):        true,
	transition(PhasePullRequest, StatusRunning, PhaseDone, StatusDone):                 true,
	transition(PhasePullRequest, StatusFailed, PhasePullRequest, StatusFailed):         true,
	transition(PhasePullRequest, StatusFailed, PhasePullRequest, StatusRunning):        true,
}

func transition(fromPhase Phase, fromStatus Status, toPhase Phase, toStatus Status) workflowTransition {
	return workflowTransition{
		from: workflowCondition{phase: fromPhase, status: fromStatus},
		to:   workflowCondition{phase: toPhase, status: toStatus},
	}
}

func allowedTransition(fromPhase Phase, fromStatus Status, toPhase Phase, toStatus Status) bool {
	return allowedTransitions[transition(fromPhase, fromStatus, toPhase, toStatus)]
}
