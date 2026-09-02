// Package app composes the concrete awdev command dependencies.
package app

import (
	"context"
	"fmt"
	"os"

	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/cli"
	"github.com/agenticworkflowdev/cli/internal/config"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	"github.com/spf13/cobra"
)

// NewCommand constructs the production awdev command.
func NewCommand() *cobra.Command {
	return cli.NewRootCommand(cli.Services{
		WorkingDirectory:       os.Getwd,
		DiscoverRoot:           gitrepo.DiscoverControllerRoot,
		ValidateExistingConfig: config.ValidateExisting,
		Initialize:             initrepo.Initialize,
		ValidateConfig: func(root string) error {
			installed, err := assets.LoadInstalled(root)
			if err != nil {
				return err
			}
			_, err = config.Parse(installed.Config.Contents)
			return err
		},
		Execute: func(_ context.Context, operation cli.Operation, item cli.SourceItem, _ string) error {
			return fmt.Errorf("awdev %s %s is not implemented yet", operation, item.Source)
		},
	})
}
