// Package app composes the concrete awdev command dependencies.
package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/agent/claudecode"
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
		RunGitHub: func(ctx context.Context, controllerRoot string, issueNumber int, options workflow.RunOptions, progress cli.ProgressReporter) (workflow.RunResult, error) {
			runtime, err := buildRuntime(controllerRoot, progress)
			if err != nil {
				return workflow.RunResult{}, err
			}
			return runtime.run.RunGitHub(ctx, controllerRoot, issueNumber, options)
		},
		ResumeGitHub: func(ctx context.Context, controllerRoot string, issueNumber int, progress cli.ProgressReporter) (workflow.ResumeResult, error) {
			runtime, err := buildRuntime(controllerRoot, progress)
			if err != nil {
				return workflow.ResumeResult{}, err
			}
			return runtime.resume.ResumeGitHub(ctx, controllerRoot, issueNumber)
		},
		RetryGitHub: func(ctx context.Context, controllerRoot string, issueNumber int, progress cli.ProgressReporter) (workflow.PublicationResult, error) {
			runtime, err := buildRuntime(controllerRoot, progress)
			if err != nil {
				return workflow.PublicationResult{}, err
			}
			return runtime.retry.RetryGitHub(ctx, controllerRoot, issueNumber)
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
	retry  *workflow.RetryService
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
	agentRunner, err := newAgentRunner(configuration, processRunner)
	if err != nil {
		return workflowRuntime{}, err
	}
	specificationRunner := &progressAgentRunner{runner: agentRunner, report: progress, interval: agentSpinnerInterval, startMessage: "● Specification agent\n  └─ writing specification..."}
	implementationRunner := &progressAgentRunner{runner: agentRunner, report: progress, interval: agentSpinnerInterval, startMessage: "● Implementation agent\n  └─ implementing changes..."}
	reviewRunner := &progressAgentRunner{runner: agentRunner, report: progress, interval: agentSpinnerInterval, startMessage: "● Review agent"}
	transitionService := state.NewTransitionService(manifestStore)
	specificationService := workflow.NewSpecificationService(
		manifestStore, transitionService, specificationPrompt, specificationRunner, resultDecoder,
		installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout, resumePrompt,
	)
	reportingSpecification := &reportingSpecificationService{service: specificationService, report: progress}
	implementationService := workflow.NewImplementationService(
		manifestStore, transitionService, implementationPrompt, fixChecksPrompt, implementationRunner, resultDecoder,
		checks.NewExecutor(processRunner), gitrepo.NewDiffInspector("git", processRunner), gitrepo.NewWorktreeInspector("git", processRunner),
		installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout, configuration.Checks, configuration.ProtectedPaths, resumePrompt,
	)
	reportingImplementation := &reportingImplementationService{service: implementationService, report: progress}
	reviewService := workflow.NewReviewService(
		manifestStore, transitionService, reviewPrompt, fixReviewPrompt, reviewRunner, reviewDecoder, resultDecoder,
		checks.NewExecutor(processRunner), gitrepo.NewDiffInspector("git", processRunner), gitrepo.NewWorktreeInspector("git", processRunner), manifestStore,
		installed.Schemas[assets.SchemaReviewResult].Path, installed.Schemas[assets.SchemaAgentResult].Path, configuration.Agent.Timeout,
		configuration.Checks, configuration.ProtectedPaths, configuration.Review.MaxAttempts, resumePrompt,
	)
	reportingReview := &reportingReviewService{service: reviewService, report: progress}
	blockers := workflow.NewBlockerService(manifestStore, transitionService, githubClient, time.Now)
	continuation := workflow.NewResumeContinuationService(reportingSpecification, reportingImplementation, reportingReview)
	publication := workflow.NewPublicationService(
		manifestStore, transitionService, manifestStore, checks.NewExecutor(processRunner), gitrepo.NewWorktreeInspector("git", processRunner),
		gitrepo.NewPublicationManager("git", processRunner), githubClient, configuration.Checks,
	)
	runService := workflow.NewRunService(
		state.NewFileLocker(), manifestReader, &reportingFetcher{fetcher: githubClient, report: progress},
		&reportingBootstrapper{bootstrapper: worktreeBootstrapper, report: progress}, manifestStore,
		state.NewWorkflowID, reportingSpecification, reportingImplementation, reportingReview, blockers,
	).WithFinalizer(publication)
	resumeService := workflow.NewResumeService(
		state.NewFileLocker(), manifestReader, transitionService, githubClient, blockers, continuation,
	).WithFinalizer(publication)
	return workflowRuntime{
		run: runService, resume: resumeService,
		retry: workflow.NewRetryService(state.NewFileLocker(), manifestReader, publication),
	}, nil
}

// newAgentRunner selects the configured provider adapter behind the
// provider-neutral agent.Runner contract.
func newAgentRunner(configuration config.Config, processRunner processrun.Runner) (agent.Runner, error) {
	switch configuration.Agent.Provider {
	case agent.ProviderCodex:
		return codex.NewRunner(configuration.Codex.Binary, processRunner)
	case agent.ProviderClaudeCode:
		return claudecode.NewRunner(configuration.ClaudeCode.Binary, configuration.ClaudeCode.Model, processRunner)
	default:
		return nil, fmt.Errorf("agent provider %q is unavailable", configuration.Agent.Provider)
	}
}

type reportingFetcher struct {
	fetcher githubapi.Fetcher
	report  cli.ProgressReporter
}

func (fetcher *reportingFetcher) Fetch(ctx context.Context, controllerRoot string, issueNumber int) (githubapi.Snapshot, error) {
	snapshot, err := fetcher.fetcher.Fetch(ctx, controllerRoot, issueNumber)
	if err == nil {
		reportMessage(fetcher.report, fmt.Sprintf("✓ Loaded GitHub issue #%d", issueNumber))
	}
	return snapshot, err
}

type reportingBootstrapper struct {
	bootstrapper workflow.Bootstrapper
	report       cli.ProgressReporter
}

func (bootstrapper *reportingBootstrapper) Continue(ctx context.Context, request workflow.Bootstrap) (gitrepo.Worktree, error) {
	worktree, err := bootstrapper.bootstrapper.Continue(ctx, request)
	if err == nil {
		reportMessage(bootstrapper.report, "✓ Created worktree "+worktree.Branch)
	}
	return worktree, err
}

type reportingSpecificationService struct {
	service *workflow.SpecificationService
	report  cli.ProgressReporter
}

func (service *reportingSpecificationService) Create(ctx context.Context, controllerRoot, workflowID string) (workflow.SpecificationResult, error) {
	result, err := service.service.Create(ctx, controllerRoot, workflowID)
	reportPhaseComplete(service.report, "Specification", result.Blocker, err)
	return result, err
}

func (service *reportingSpecificationService) Resume(ctx context.Context, controllerRoot, workflowID string) (workflow.SpecificationResult, error) {
	result, err := service.service.Resume(ctx, controllerRoot, workflowID)
	reportPhaseComplete(service.report, "Specification", result.Blocker, err)
	return result, err
}

type reportingImplementationService struct {
	service *workflow.ImplementationService
	report  cli.ProgressReporter
}

func (service *reportingImplementationService) Implement(ctx context.Context, controllerRoot, workflowID string) (workflow.ImplementationResult, error) {
	result, err := service.service.Implement(ctx, controllerRoot, workflowID)
	reportPhaseComplete(service.report, "Implementation", result.Blocker, err)
	return result, err
}

func (service *reportingImplementationService) Resume(ctx context.Context, controllerRoot, workflowID string) (workflow.ImplementationResult, error) {
	result, err := service.service.Resume(ctx, controllerRoot, workflowID)
	reportPhaseComplete(service.report, "Implementation", result.Blocker, err)
	return result, err
}

type reportingReviewService struct {
	service *workflow.ReviewService
	report  cli.ProgressReporter
}

func (service *reportingReviewService) Review(ctx context.Context, controllerRoot, workflowID string, results []checks.Result, baseline gitrepo.WorktreeBaseline) (workflow.ReviewResult, error) {
	result, err := service.service.Review(ctx, controllerRoot, workflowID, results, baseline)
	reportPhaseComplete(service.report, "Review", result.Blocker, err)
	return result, err
}

func (service *reportingReviewService) Resume(ctx context.Context, controllerRoot, workflowID string) (workflow.ReviewResult, error) {
	result, err := service.service.Resume(ctx, controllerRoot, workflowID)
	reportPhaseComplete(service.report, "Review", result.Blocker, err)
	return result, err
}

func reportPhaseComplete(report cli.ProgressReporter, phase string, blocker *workflow.BlockerRequest, err error) {
	if err == nil && blocker == nil {
		reportMessage(report, "\n✓ "+phase+" complete")
	}
}

func reportMessage(report cli.ProgressReporter, message string) {
	if report != nil {
		report(cli.ProgressUpdate{Message: message})
	}
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
		message = "● Agent\n  └─ working..."
	}
	runner.startOnce.Do(func() {
		runner.report(cli.ProgressUpdate{Message: message})
	})
	upstreamProgress := request.Progress
	request.Progress = func(event agent.ProgressEvent) {
		if upstreamProgress != nil {
			upstreamProgress(event)
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
