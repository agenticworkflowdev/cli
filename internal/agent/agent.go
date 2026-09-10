package agent

import (
	"context"
	"encoding/json"
)

// AccessLevel is the filesystem access granted to one agent run.
type AccessLevel string

const (
	AccessReadOnly       AccessLevel = "read-only"
	AccessWorkspaceWrite AccessLevel = "workspace-write"
)

// Request is the provider-neutral contract for one agent turn. A non-empty
// ResumeSessionID continues that provider session instead of creating a new
// one.
type Request struct {
	Worktree        string
	Prompt          string
	OutputSchema    string
	OutputDirectory string
	Access          AccessLevel
	ResumeSessionID string
	Progress        func(ProgressEvent)
}

// ProgressKind identifies a provider-neutral event suitable for live display.
type ProgressKind string

const (
	ProgressMessage       ProgressKind = "message"
	ProgressReasoning     ProgressKind = "reasoning"
	ProgressCommand       ProgressKind = "command"
	ProgressCommandOutput ProgressKind = "command_output"
)

// ProgressEvent captures stable progress metadata without coupling callers to
// provider-specific event payloads.
type ProgressEvent struct {
	Type     string
	Kind     ProgressKind
	Message  string
	ExitCode *int
}

// RunResult contains the authoritative final output and observed progress.
type RunResult struct {
	FinalOutput json.RawMessage
	SessionID   string
	Progress    []ProgressEvent
}

// Runner starts a fresh provider session or resumes Request.ResumeSessionID.
type Runner interface {
	Run(context.Context, Request) (RunResult, error)
}

// ProviderRunner exposes the provider identity needed to validate durable
// session ownership without coupling workflow code to a concrete adapter.
type ProviderRunner interface {
	Runner
	Provider() Provider
}

// OutcomeStatus is the semantic status returned by the agent.
type OutcomeStatus string

const (
	OutcomeCompleted OutcomeStatus = "completed"
	OutcomeBlocked   OutcomeStatus = "blocked"
)

// Outcome is a schema-validated and semantically valid agent result.
type Outcome struct {
	Status   OutcomeStatus `json:"status"`
	Summary  string        `json:"summary,omitempty"`
	Question string        `json:"question,omitempty"`
}
