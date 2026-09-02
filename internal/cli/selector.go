package cli

import (
	"fmt"

	"charm.land/huh/v2"
	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/spf13/cobra"
)

func selectAgent(command *cobra.Command) (agent.Provider, error) {
	var selected agent.Provider
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[agent.Provider]().
				Title("Select the AI:").
				Options(
					huh.NewOption("Claude Code", agent.ProviderClaudeCode),
					huh.NewOption("Codex", agent.ProviderCodex),
				).
				Value(&selected),
		),
	).
		WithInput(command.InOrStdin()).
		WithOutput(command.OutOrStdout())

	if err := form.RunWithContext(command.Context()); err != nil {
		return "", fmt.Errorf("select agent: %w", err)
	}
	return selected, nil
}
