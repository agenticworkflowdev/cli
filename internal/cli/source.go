package cli

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/agenticworkflowdev/cli/internal/workflow"
	"github.com/spf13/cobra"
)

func newSourceCommand(operation Operation, services Services) *cobra.Command {
	command := &cobra.Command{
		Use:     string(operation) + " SOURCE NUMBER",
		Short:   sourceCommandDescription(operation),
		Example: "  awdev " + string(operation) + " github NUMBER",
		Args: func(_ *cobra.Command, args []string) error {
			_, err := parseSourceItem(operation, args)
			return err
		},
		RunE: func(command *cobra.Command, args []string) error {
			if (operation == OperationRun || operation == OperationResume) && services.Getenv("AWDEV_WORKER") == "1" {
				return fmt.Errorf("awdev %s cannot be invoked from an awdev worker", operation)
			}

			root, err := discoverRoot(command.Context(), services)
			if err != nil {
				return err
			}
			if services.ValidateConfig == nil {
				return errors.New("configuration validation is unavailable")
			}
			if err := services.ValidateConfig(root); err != nil {
				return fmt.Errorf("validate configuration: %w", err)
			}
			item, err := parseSourceItem(operation, args)
			if err != nil {
				return err
			}
			if operation == OperationRun && item.Source == SourceGitHub && services.RunGitHub != nil {
				result, err := services.RunGitHub(command.Context(), root, item.Number)
				if err != nil {
					return err
				}
				return renderGitHubRun(command, result)
			}
			if services.Execute == nil {
				return fmt.Errorf("awdev %s github is not implemented yet", operation)
			}
			return services.Execute(command.Context(), operation, item, root)
		},
	}
	command.Flags().SetInterspersed(false)
	return command
}

func renderGitHubRun(command *cobra.Command, result workflow.RunResult) error {
	if result.Outcome == workflow.RunExisting {
		_, err := fmt.Fprintf(
			command.OutOrStdout(),
			"Workflow %s already exists: %s/%s.\n",
			result.WorkflowID,
			result.Existing.Phase,
			result.Existing.Status,
		)
		return err
	}
	_, err := fmt.Fprintf(
		command.OutOrStdout(),
		"Validated GitHub issue %s#%d on %s as %s.\n",
		result.Snapshot.Repository.NameWithOwner,
		result.Snapshot.Issue.Number,
		result.Snapshot.Repository.DefaultBranch,
		result.Snapshot.Actor.Login,
	)
	return err
}

func parseSourceItem(operation Operation, args []string) (SourceItem, error) {
	if len(args) < 2 {
		return SourceItem{}, sourceUsageError(operation, fmt.Sprintf("%s requires a source and issue number", operation))
	}
	if len(args) > 2 {
		return SourceItem{}, sourceUsageError(operation, fmt.Sprintf("accepts 2 arg(s), received %d", len(args)))
	}
	if !isDecimal(args[1]) {
		return SourceItem{}, sourceUsageError(operation, "issue number must be a positive decimal integer")
	}
	number, err := strconv.Atoi(args[1])
	if err != nil || number <= 0 {
		return SourceItem{}, sourceUsageError(operation, "issue number must be a positive decimal integer")
	}

	source := Source(args[0])
	switch source {
	case SourceLinear:
		return SourceItem{}, errors.New("Linear is not implemented yet.")
	case SourceGitHub:
		return SourceItem{Source: source, Number: number}, nil
	default:
		return SourceItem{}, sourceUsageError(operation, fmt.Sprintf("unsupported source %q", args[0]))
	}
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func sourceUsageError(operation Operation, message string) error {
	return fmt.Errorf("%s\nUsage: awdev %s SOURCE NUMBER", message, operation)
}

func sourceCommandDescription(operation Operation) string {
	switch operation {
	case OperationRun:
		return "Start a workflow for a source item"
	case OperationStatus:
		return "Show workflow status for a source item"
	case OperationResume:
		return "Resume a blocked workflow for a source item"
	case OperationRetry:
		return "Retry a supported workflow action for a source item"
	default:
		return "Operate on a source item"
	}
}
