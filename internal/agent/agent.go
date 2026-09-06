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

// Request is the provider-neutral contract for one fresh agent run.
type Request struct {
	Worktree        string
	Prompt          string
	OutputSchema    string
	OutputDirectory string
	Access          AccessLevel
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

// Runner starts one fresh provider session.
type Runner interface {
	Run(context.Context, Request) (RunResult, error)
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
