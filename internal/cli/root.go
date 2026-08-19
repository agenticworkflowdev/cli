package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewRootCommand creates the awdev root command.
func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "awdev",
		Short:         "Agentic Workflow Development CLI",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(command.OutOrStdout(), "Hello World")
			return err
		},
	}
}
