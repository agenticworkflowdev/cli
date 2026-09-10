package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	"github.com/spf13/cobra"
)

func newInitCommand(services Services) *cobra.Command {
	command := &cobra.Command{
		Use:   "init",
		Short: "Select an agent and initialize the current repository",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			provider, withSkill, err := selectInitOptions(command)
			if err != nil {
				return err
			}
			root, err := discoverRoot(command.Context(), services)
			if err != nil {
				return err
			}
			if services.ValidateExistingConfig != nil {
				if err := services.ValidateExistingConfig(root); err != nil {
					return fmt.Errorf("validate existing configuration: %w", err)
				}
			}
			if services.Initialize == nil {
				return errors.New("repository initialization is unavailable")
			}
			result, err := services.Initialize(root, provider, initrepo.Options{WithSkill: withSkill})
			if err != nil {
				return fmt.Errorf("initialize repository: %w", err)
			}
			if services.ValidateConfig != nil {
				if err := services.ValidateConfig(root); err != nil {
					return fmt.Errorf("validate configuration: %w", err)
				}
			}
			return writeInitializationResult(command, result, provider)
		},
	}
	return command
}

func writeInitializationResult(command *cobra.Command, result initrepo.Result, provider agent.Provider) error {
	writer := command.OutOrStdout()
	if result.AwdevDirectoryCreated {
		if _, err := fmt.Fprintln(writer, "Created .awdev/"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(writer, ".awdev/ already exists; missing defaults were checked."); err != nil {
		return err
	}

	switch result.Gitignore {
	case initrepo.GitignoreCreated:
		if _, err := fmt.Fprintln(writer, "Created .gitignore with awdev state and worktree entries."); err != nil {
			return err
		}
	case initrepo.GitignoreUpdated:
		if _, err := fmt.Fprintln(writer, "Updated .gitignore with awdev state and worktree entries."); err != nil {
			return err
		}
	case initrepo.GitignoreRetained:
		if _, err := fmt.Fprintln(writer, ".gitignore already contains the AWDev entries."); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown .gitignore initialization status %q", result.Gitignore)
	}
	if result.Skill != nil {
		if err := writeSkillFileResult(writer, ".agents/skills/awdev/SKILL.md", result.Skill.Instructions); err != nil {
			return err
		}
		metadataPath := result.Skill.MetadataPath
		if metadataPath == "" {
			metadataPath = ".agents/skills/awdev/agents/openai.yaml"
		}
		if err := writeSkillFileResult(writer, metadataPath, result.Skill.Metadata); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(writer, "Initialization complete. AWDev is configured to use %s.\n", provider.DisplayName())
	return err
}

func writeSkillFileResult(writer io.Writer, path string, status initrepo.FileStatus) error {
	switch status {
	case initrepo.FileCreated:
		_, err := fmt.Fprintf(writer, "Created %s.\n", path)
		return err
	case initrepo.FileRetained:
		_, err := fmt.Fprintf(writer, "%s already exists; retained unchanged.\n", path)
		return err
	default:
		return fmt.Errorf("unknown skill file initialization status %q", status)
	}
}
