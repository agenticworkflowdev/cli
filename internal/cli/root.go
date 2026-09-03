package cli

import (
	"context"
	"os"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	"github.com/agenticworkflowdev/cli/internal/workflow"
	"github.com/spf13/cobra"
)

// Operation identifies a source-scoped CLI action.
type Operation string

const (
	OperationRun    Operation = "run"
	OperationStatus Operation = "status"
	OperationResume Operation = "resume"
	OperationRetry  Operation = "retry"
)

// Source identifies an issue provider reserved by the command grammar.
type Source string

const (
	SourceGitHub Source = "github"
	SourceLinear Source = "linear"
)

// SourceItem is one validated source-scoped issue identity.
type SourceItem struct {
	Source Source
	Number int
}

// ProgressReporter renders human-readable progress for a long-running command.
type ProgressReporter func(string)

// Services contains the side-effecting seams used by the CLI.
type Services struct {
	WorkingDirectory       func() (string, error)
	DiscoverRoot           func(context.Context, string) (string, error)
	ValidateExistingConfig func(string) error
	Initialize             func(string, agent.Provider) (initrepo.Result, error)
	ValidateConfig         func(string) error
	RunGitHub              func(context.Context, string, int, ProgressReporter) (workflow.RunResult, error)
	StatusGitHub           func(context.Context, string, int, workflow.StatusOptions) (workflow.StatusResult, error)
	Execute                func(context.Context, Operation, SourceItem, string) error
	Getenv                 func(string) string
}

// NewRootCommand creates the awdev root command.
func NewRootCommand(services Services) *cobra.Command {
	if services.Getenv == nil {
		services.Getenv = os.Getenv
	}

	command := &cobra.Command{
		Use:           "awdev",
		Short:         "Agentic Workflow Development CLI",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newInitCommand(services))
	for _, operation := range []Operation{OperationRun, OperationStatus, OperationResume, OperationRetry} {
		command.AddCommand(newSourceCommand(operation, services))
	}
	return command
}
