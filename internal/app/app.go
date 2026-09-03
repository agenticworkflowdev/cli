// Package app composes the concrete awdev command dependencies.
package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/agent/codex"
	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/cli"
	"github.com/agenticworkflowdev/cli/internal/config"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
	"github.com/spf13/cobra"
)

const agentHeartbeatInterval = 15 * time.Second

// NewCommand constructs the production awdev command.
func NewCommand() *cobra.Command {
	processRunner := processrun.NewRunner()
	githubClient := githubapi.NewClient("gh", processRunner)
	worktreeManager := gitrepo.NewWorktreeManager("git", processRunner)
	worktreeBootstrapper := workflow.NewWorktreeBootstrapper(worktreeManager)
	manifestStore := state.NewStore()
	manifestReader := state.NewManifestReader()
	statusService := workflow.NewStatusService(manifestReader, githubClient)
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
		RunGitHub: func(ctx context.Context, controllerRoot string, issueNumber int, progress cli.ProgressReporter) (workflow.RunResult, error) {
			installed, err := assets.LoadInstalled(controllerRoot)
			if err != nil {
				return workflow.RunResult{}, err
			}
			configuration, err := config.Parse(installed.Config.Contents)
			if err != nil {
				return workflow.RunResult{}, err
			}
			if configuration.Agent.Provider != agent.ProviderCodex {
				return workflow.RunResult{}, fmt.Errorf("agent provider %q is unavailable", configuration.Agent.Provider)
			}
			promptRenderer, err := prompt.NewRenderer(assets.PromptSpec, installed.Prompts[assets.PromptSpec].Contents)
			if err != nil {
				return workflow.RunResult{}, err
			}
			resultDecoder, err := agent.NewResultDecoder(installed.Schemas[assets.SchemaAgentResult].Contents)
			if err != nil {
				return workflow.RunResult{}, err
			}
			codexRunner, err := codex.NewRunner(configuration.Codex.Binary, processRunner)
			if err != nil {
				return workflow.RunResult{}, err
			}
			notifyingRunner := &progressAgentRunner{runner: codexRunner, report: progress, interval: agentHeartbeatInterval}
			transitionService := state.NewTransitionService(manifestStore)
			specificationService := workflow.NewSpecificationService(
				manifestStore,
				transitionService,
				promptRenderer,
				notifyingRunner,
				resultDecoder,
				installed.Schemas[assets.SchemaAgentResult].Path,
				configuration.Agent.Timeout,
			)
			runService := workflow.NewRunService(state.NewFileLocker(), manifestReader, githubClient, worktreeBootstrapper, manifestStore, state.NewWorkflowID, specificationService)
			return runService.RunGitHub(ctx, controllerRoot, issueNumber)
		},
		StatusGitHub: statusService.StatusGitHub,
		Execute: func(_ context.Context, operation cli.Operation, item cli.SourceItem, _ string) error {
			return fmt.Errorf("awdev %s %s is not implemented yet", operation, item.Source)
		},
	})
}

type progressAgentRunner struct {
	runner   agent.Runner
	report   func(string)
	interval time.Duration
}

func (runner *progressAgentRunner) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	if runner == nil || runner.runner == nil {
		return agent.RunResult{}, fmt.Errorf("progress agent runner is not configured")
	}
	if runner.report == nil {
		return runner.runner.Run(ctx, request)
	}
	runner.report("Creating specification with Codex. This can take a few minutes...")
	if runner.interval <= 0 {
		return runner.runner.Run(ctx, request)
	}

	ticker := time.NewTicker(runner.interval)
	done := make(chan struct{})
	var reporting sync.WaitGroup
	reporting.Add(1)
	go func() {
		defer reporting.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runner.report("Codex is still working...")
			case <-done:
				return
			}
		}
	}()

	result, err := runner.runner.Run(ctx, request)
	close(done)
	reporting.Wait()
	return result, err
}
