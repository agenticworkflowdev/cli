package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
	"github.com/spf13/cobra"
)

func newSourceCommand(operation Operation, services Services) *cobra.Command {
	var jsonOutput bool
	var checkIssue bool
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
			if operation == OperationStatus && item.Source == SourceGitHub && services.StatusGitHub != nil {
				result, err := services.StatusGitHub(command.Context(), root, item.Number, workflow.StatusOptions{CheckIssue: checkIssue})
				if err != nil {
					return err
				}
				return renderGitHubStatus(command, result, jsonOutput)
			}
			if services.Execute == nil {
				return fmt.Errorf("awdev %s github is not implemented yet", operation)
			}
			return services.Execute(command.Context(), operation, item, root)
		},
	}
	if operation == OperationStatus {
		command.Flags().BoolVar(&jsonOutput, "json", false, "emit stable machine-readable JSON")
		command.Flags().BoolVar(&checkIssue, "check-issue", false, "warn if the live issue changed")
		command.Flags().SetInterspersed(true)
	} else {
		command.Flags().SetInterspersed(false)
	}
	return command
}

func renderGitHubRun(command *cobra.Command, result workflow.RunResult) error {
	if result.Outcome == workflow.RunExisting {
		if result.Existing.Manifest == nil {
			return errors.New("existing workflow result is missing its manifest")
		}
		_, err := fmt.Fprintf(
			command.OutOrStdout(),
			"Workflow %s already exists: %s/%s.\n",
			result.WorkflowID,
			result.Existing.Manifest.Phase,
			result.Existing.Manifest.Status,
		)
		return err
	}
	_, err := fmt.Fprintf(
		command.OutOrStdout(),
		"GitHub issue: #%d\nWorktree: %s\nBranch: %s\n",
		result.Snapshot.Issue.Number,
		filepath.Base(result.Worktree.AbsolutePath),
		result.Worktree.Branch,
	)
	return err
}

type publicIssueStatus struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

type publicWorkflowStatus struct {
	WorkflowID      string            `json:"workflow_id"`
	Source          state.Source      `json:"source"`
	Repository      string            `json:"repository"`
	Issue           publicIssueStatus `json:"issue"`
	Phase           state.Phase       `json:"phase"`
	Status          state.Status      `json:"status"`
	BlockerQuestion string            `json:"blocker_question,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	PullRequestURL  string            `json:"pull_request_url,omitempty"`
	Warning         string            `json:"warning,omitempty"`
}

func renderGitHubStatus(command *cobra.Command, result workflow.StatusResult, jsonOutput bool) error {
	manifest := result.Manifest
	public := publicWorkflowStatus{
		WorkflowID: manifest.WorkflowID,
		Source:     manifest.Source,
		Repository: manifest.Repository,
		Issue: publicIssueStatus{
			Number: manifest.Issue.Number,
			Title:  manifest.Issue.Title,
			URL:    manifest.Issue.URL,
		},
		Phase:   manifest.Phase,
		Status:  manifest.Status,
		Warning: result.Warning,
	}
	if manifest.Blocker != nil {
		public.BlockerQuestion = manifest.Blocker.Question
	}
	if manifest.LastError != nil {
		public.LastError = manifest.LastError.Message
	}
	if manifest.PullRequest != nil {
		public.PullRequestURL = manifest.PullRequest.URL
	}
	if jsonOutput {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(public)
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Workflow: %s\n", public.WorkflowID)
	fmt.Fprintf(&output, "Source: %s\n", public.Source)
	fmt.Fprintf(&output, "Issue: %s#%d — %s\n", public.Repository, public.Issue.Number, sanitizeHumanText(public.Issue.Title))
	fmt.Fprintf(&output, "Phase: %s\n", public.Phase)
	fmt.Fprintf(&output, "Status: %s\n", public.Status)
	if public.BlockerQuestion != "" {
		fmt.Fprintf(&output, "Blocker: %s\n", sanitizeHumanText(public.BlockerQuestion))
	}
	if public.LastError != "" {
		fmt.Fprintf(&output, "Last error: %s\n", sanitizeHumanText(public.LastError))
	}
	if public.PullRequestURL != "" {
		fmt.Fprintf(&output, "Pull request: %s\n", public.PullRequestURL)
	}
	if public.Warning != "" {
		fmt.Fprintf(&output, "Warning: %s\n", sanitizeHumanText(public.Warning))
	}
	_, err := fmt.Fprint(command.OutOrStdout(), output.String())
	return err
}

func sanitizeHumanText(value string) string {
	var safe strings.Builder
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index++
			if index < len(value) && value[index] == '[' {
				index++
				for index < len(value) {
					final := value[index]
					index++
					if final >= 0x40 && final <= 0x7e {
						break
					}
				}
			} else if index < len(value) {
				index++
			}
			continue
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if unicode.IsControl(character) {
			safe.WriteByte(' ')
			continue
		}
		safe.WriteRune(character)
	}
	return strings.Join(strings.Fields(safe.String()), " ")
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
