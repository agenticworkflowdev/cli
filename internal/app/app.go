// Package app composes the concrete awdev command dependencies.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/agent/codex"
	"github.com/agenticworkflowdev/cli/internal/assets"
	"github.com/agenticworkflowdev/cli/internal/checks"
	"github.com/agenticworkflowdev/cli/internal/cli"
	"github.com/agenticworkflowdev/cli/internal/config"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	processrun "github.com/agenticworkflowdev/cli/internal/process"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
	"github.com/spf13/cobra"
)

const agentSpinnerInterval = 100 * time.Millisecond

var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// NewCommand constructs the production awdev command.
func NewCommand() *cobra.Command {
	processRunner := processrun.NewRunner()
	githubClient := githubapi.NewClient("gh", processRunner)
	worktreeManager := gitrepo.NewWorktreeManager("git", processRunner)
	worktreeBootstrapper := workflow.NewWorktreeBootstrapper(worktreeManager)
	manifestStore := state.NewStore()
	manifestReader := state.NewManifestReader()
	statusService := workflow.NewStatusService(manifestReader, githubClient)
	buildRuntime := func(controllerRoot string, progress cli.ProgressReporter) (workflowRuntime, error) {
		return newWorkflowRuntime(controllerRoot, progress, processRunner, githubClient, worktreeBootstrapper, manifestStore, manifestReader)
	}
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
			runtime, err := buildRuntime(controllerRoot, progress)
			if err != nil {
				return workflow.RunResult{}, err
			}
			return runtime.run.RunGitHub(ctx, controllerRoot, issueNumber)
		},
		ResumeGitHub: func(ctx context.Context, controllerRoot string, issueNumber int, progress cli.ProgressReporter) (workflow.ResumeResult, error) {
			runtime, err := buildRuntime(controllerRoot, progress)
			if err != nil {
				return workflow.ResumeResult{}, err
			}
			return runtime.resume.ResumeGitHub(ctx, controllerRoot, issueNumber)
		},
		StatusGitHub: statusService.StatusGitHub,
		Execute: func(_ context.Context, operation cli.Operation, item cli.SourceItem, _ string) error {
			return fmt.Errorf("awdev %s %s is not implemented yet", operation, item.Source)
		},
	})
}

type workflowRuntime struct {
	run    *workflow.RunService
	resume *workflow.ResumeService
}

func newWorkflowRuntime(
	controllerRoot string,
	progress cli.ProgressReporter,
	processRunner processrun.Runner,
	githubClient *githubapi.Client,
	worktreeBootstrapper workflow.Bootstrapper,
	manifestStore *state.Store,
	manifestReader state.ManifestReader,
) (workflowRuntime, error) {
	installed, err := assets.LoadInstalled(controllerRoot)
	if err != nil {
		return workflowRuntime{}, err
	}
	configuration, err := config.Parse(installed.Config.Contents)
	if err != nil {
		return workflowRuntime{}, err
	}
	if configuration.Agent.Provider != agent.ProviderCodex {
		return workflowRuntime{}, fmt.Errorf("agent provider %q is unavailable", configuration.Agent.Provider)
	}
	render := func(name string) (*prompt.Renderer, error) {
		return prompt.NewRenderer(name, installed.Prompts[name].Contents)
	}
	specificationPrompt, err := render(assets.PromptSpec)
	if err != nil {
		return workflowRuntime{}, err
	}
	implementationPrompt, err := render(assets.PromptImplement)
	if err != nil {
		return workflowRuntime{}, err
	}
	fixChecksPrompt, err := render(assets.PromptFixChecks)
	if err != nil {
		return workflowRuntime{}, err
	}
	reviewPrompt, err := render(assets.PromptReview)
	if err != nil {
		return workflowRuntime{}, err
	}
	fixReviewPrompt, err := render(assets.PromptFixReview)
	if err != nil {
		return workflowRuntime{}, err
	}
	resumePrompt, err := render(assets.PromptResume)
	if err != nil {
		return workflowRuntime{}, err
	}
	resultDecoder, err := agent.NewResultDecoder(installed.Schemas[assets.SchemaAgentResult].Contents)
	if err != nil {
		return workflowRuntime{}, err
	}
	reviewDecoder, err := review.NewResultDecoder(installed.Schemas[assets.SchemaReviewResult].Contents)
	if err != nil {
		return workflowRuntime{}, err
	}
	codexRunner, err := codex.NewRunner(configuration.Codex.Binary, processRunner)
	if err != nil {
		return workflowRuntime{}, err
	}
	specificationRunner := &progressAgentRunner{runner: codexRunner, report: progress, interval: agentSpinnerInterval}
	implementationRunner := &progressAgentRunner{runner: codexRunner, report: progress, interval: agentSpinnerInterval, startMessage: "Implementing the specification. This can take a few moments..."}
	reviewRunner := &progressAgentRunner{runner: codexRunner, report: progress, interval: agentSpinnerInterval, startMessage: "Reviewing the implementation. This can take a few moments..."}
	transitionService := state.NewTransitionService(manifestStore)
	specificationService := workflow.NewSpecificationService(
		manifestStore, transitionService, specificationPrompt, specificationRunner, resultDecoder,
		installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout, resumePrompt,
	)
	reportingSpecification := &reportingSpecificationCreator{creator: specificationService, report: progress}
	implementationService := workflow.NewImplementationService(
		manifestStore, transitionService, implementationPrompt, fixChecksPrompt, implementationRunner, resultDecoder,
		checks.NewExecutor(processRunner), gitrepo.NewDiffInspector("git", processRunner), gitrepo.NewWorktreeInspector("git", processRunner),
		installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout, configuration.Checks, configuration.ProtectedPaths, resumePrompt,
	)
	reviewService := workflow.NewReviewService(
		manifestStore, transitionService, reviewPrompt, fixReviewPrompt, reviewRunner, reviewDecoder, resultDecoder,
		checks.NewExecutor(processRunner), gitrepo.NewDiffInspector("git", processRunner), gitrepo.NewWorktreeInspector("git", processRunner), manifestStore,
		installed.Schemas[assets.SchemaReviewResult].Path, installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout,
		configuration.Checks, configuration.ProtectedPaths, configuration.Review.MaxAttempts, resumePrompt,
	)
	blockers := workflow.NewBlockerService(manifestStore, transitionService, githubClient, time.Now)
	continuation := workflow.NewResumeContinuationService(specificationService, implementationService, reviewService)
	return workflowRuntime{
		run: workflow.NewRunService(
			state.NewFileLocker(), manifestReader, githubClient, worktreeBootstrapper, manifestStore,
			state.NewWorkflowID, reportingSpecification, implementationService, reviewService, blockers,
		),
		resume: workflow.NewResumeService(
			state.NewFileLocker(), manifestReader, transitionService, githubClient, blockers, continuation,
		),
	}, nil
}

type reportingSpecificationCreator struct {
	creator workflow.SpecificationCreator
	report  cli.ProgressReporter
}

func (creator *reportingSpecificationCreator) Create(ctx context.Context, controllerRoot, workflowID string) (workflow.SpecificationResult, error) {
	result, err := creator.creator.Create(ctx, controllerRoot, workflowID)
	if err != nil || result.Blocker != nil || result.Manifest.SpecificationPath == "" || creator.report == nil {
		return result, err
	}
	specificationPath := result.Manifest.SpecificationPath
	absolutePath := filepath.Join(
		controllerRoot,
		filepath.FromSlash(result.Manifest.Worktree),
		filepath.FromSlash(specificationPath),
	)
	creator.report(cli.ProgressUpdate{
		Message: path.Base(specificationPath),
		URL:     (&url.URL{Scheme: "file", Path: absolutePath}).String(),
	})
	return result, nil
}

type progressAgentRunner struct {
	runner       agent.Runner
	report       cli.ProgressReporter
	interval     time.Duration
	startMessage string
	startOnce    sync.Once
}

func (runner *progressAgentRunner) Run(ctx context.Context, request agent.Request) (agent.RunResult, error) {
	if runner == nil || runner.runner == nil {
		return agent.RunResult{}, fmt.Errorf("progress agent runner is not configured")
	}
	if runner.report == nil {
		return runner.runner.Run(ctx, request)
	}
	message := runner.startMessage
	if message == "" {
		message = "Creating specification. This can take a few moments..."
	}
	runner.startOnce.Do(func() {
		runner.report(cli.ProgressUpdate{Message: message})
	})
	upstreamProgress := request.Progress
	request.Progress = func(event agent.ProgressEvent) {
		if upstreamProgress != nil {
			upstreamProgress(event)
		}
		if message := readableAgentProgress(event); message != "" {
			runner.report(cli.ProgressUpdate{Message: message, Untrusted: true})
		}
	}
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
		frame := 0
		for {
			select {
			case <-ticker.C:
				runner.report(cli.ProgressUpdate{Message: spinnerFrames[frame], Transient: true})
				frame = (frame + 1) % len(spinnerFrames)
			case <-done:
				runner.report(cli.ProgressUpdate{Transient: true})
				return
			}
		}
	}()

	result, err := runner.runner.Run(ctx, request)
	close(done)
	reporting.Wait()
	return result, err
}

func readableAgentProgress(event agent.ProgressEvent) string {
	message := strings.TrimRight(event.Message, "\r\n")
	switch event.Kind {
	case agent.ProgressMessage:
		if message != "" {
			var outcome agent.Outcome
			if json.Unmarshal([]byte(message), &outcome) == nil {
				switch {
				case outcome.Status == agent.OutcomeCompleted && outcome.Summary != "":
					message = outcome.Summary
				case outcome.Status == agent.OutcomeBlocked && outcome.Question != "":
					return prefixProgressLines("Agent question: ", outcome.Question)
				}
			}
			return prefixProgressLines("Agent: ", message)
		}
	case agent.ProgressReasoning:
		if message != "" {
			return prefixProgressLines("Reasoning: ", message)
		}
	case agent.ProgressCommand:
		if message != "" {
			return prefixProgressLines("Command: ", message)
		}
	case agent.ProgressCommandOutput:
		var output strings.Builder
		if message != "" {
			output.WriteString("Command output:\n")
			output.WriteString(prefixProgressLines("│ ", message))
		}
		if event.ExitCode != nil {
			if output.Len() > 0 {
				output.WriteByte('\n')
			}
			fmt.Fprintf(&output, "Command exit code: %d", *event.ExitCode)
		}
		return output.String()
	}
	return ""
}

func prefixProgressLines(prefix, message string) string {
	lines := strings.Split(message, "\n")
	for index := range lines {
		lines[index] = prefix + lines[index]
	}
	return strings.Join(lines, "\n")
}
