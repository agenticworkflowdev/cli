package cli

import (
	"fmt"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// NewRootCommand creates the awdev root command.
func NewRootCommand(selectedAgent *string) *cobra.Command {
	return &cobra.Command{
		Use:           "awdev",
		Short:         "Agentic Workflow Development CLI",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Select the AI:").
						Options(
							huh.NewOption("Claude Code", "Claude Code"),
							huh.NewOption("Codex", "Codex"),
						).
						Value(selectedAgent),
				),
			).
				WithInput(command.InOrStdin()).
				WithOutput(command.OutOrStdout())

			if err := form.Run(); err != nil {
				return fmt.Errorf("select agent: %w", err)
			}

			return nil
		},
	}
}
