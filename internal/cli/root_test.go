package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/cli"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	reviewapi "github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

const cliTestWorkflowID = "wf_0123456789abcdef0123456789abcdef"

func TestRootCommandShowsHelpWithoutSelectingAnAgent(t *testing.T) {
	var output bytes.Buffer
	command := cli.NewRootCommand(cli.Services{})
	command.SetOut(&output)
	command.SetArgs(nil)

	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}
	for _, want := range []string{"Usage:", "awdev [command]", "init", "run"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("root output %q does not contain %q", output.String(), want)
		}
	}
	if strings.Contains(output.String(), "Select the AI:") {
		t.Errorf("root output unexpectedly contains agent selector: %q", output.String())
	}
}

func TestInitCommandOffersAndHandlesBothAgents(t *testing.T) {
	t.Setenv("TERM", "dumb")

	tests := []struct {
		name       string
		input      string
		wantAgent  string
		wantOutput string
	}{
		{name: "Codex", input: "2\n", wantAgent: "Codex", wantOutput: "Initialization complete. AWDev is configured to use Codex.\n"},
		{name: "Claude Code", input: "1\n", wantAgent: "Claude Code", wantOutput: "Claude Code is not implemented yet.\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			servicesCalled := false
			var initializedProvider agent.Provider
			services := cli.Services{
				WorkingDirectory: func() (string, error) { servicesCalled = true; return "/repo", nil },
				DiscoverRoot:     func(context.Context, string) (string, error) { servicesCalled = true; return "/repo", nil },
				Initialize: func(_ string, provider agent.Provider) (initrepo.Result, error) {
					servicesCalled = true
					initializedProvider = provider
					return initrepo.Result{AwdevDirectoryCreated: true, Gitignore: initrepo.GitignoreCreated}, nil
				},
				ValidateConfig: func(string) error { servicesCalled = true; return nil },
			}
			command := cli.NewRootCommand(services)
			command.SetIn(strings.NewReader(test.input))
			command.SetOut(&output)
			command.SetArgs([]string{"init"})

			if err := command.Execute(); err != nil {
				t.Fatalf("execute command: %v", err)
			}

			if !strings.Contains(output.String(), "Select the AI:") {
				t.Errorf("output %q does not contain prompt title", output.String())
			}
			for _, option := range []string{"Claude Code", "Codex"} {
				if !strings.Contains(output.String(), option) {
					t.Errorf("output %q does not contain option %q", output.String(), option)
				}
			}
			if test.wantOutput != "" && !strings.Contains(output.String(), test.wantOutput) {
				t.Errorf("output %q does not contain exact unavailable message %q", output.String(), test.wantOutput)
			}
			if test.wantAgent == "Claude Code" && servicesCalled {
				t.Fatal("Claude Code selection performed repository side effects")
			}
			if test.wantAgent == "Codex" && !servicesCalled {
				t.Fatal("Codex selection did not initialize the repository")
			}
			if test.wantAgent == "Codex" && initializedProvider != agent.ProviderCodex {
				t.Fatalf("initialized provider = %q, want codex", initializedProvider)
			}
		})
	}
}

func TestInitCommandReportsSelectorCancellationBeforeServices(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	servicesCalled := false
	services := cli.Services{WorkingDirectory: func() (string, error) { servicesCalled = true; return "", nil }}
	command := cli.NewRootCommand(services)
	command.SetIn(strings.NewReader("\x03"))
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"init"})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "select agent") {
		t.Fatalf("execute error = %v, want selector error", err)
	}
	if servicesCalled {
		t.Fatal("repository service called after selector cancellation")
	}
}

func TestInitCommandPrintsConciseSummary(t *testing.T) {
	t.Setenv("TERM", "dumb")

	tests := []struct {
		name      string
		result    initrepo.Result
		want      []string
		wantBlock string
	}{
		{
			name:   "created",
			result: initrepo.Result{AwdevDirectoryCreated: true, Gitignore: initrepo.GitignoreCreated},
			want:   []string{"Created .awdev/\n", "Created .gitignore with awdev state and worktree entries."},
		},
		{
			name:      "created with retained gitignore",
			result:    initrepo.Result{AwdevDirectoryCreated: true, Gitignore: initrepo.GitignoreRetained},
			wantBlock: "Created .awdev/\n.gitignore already contains the AWDev entries.\nInitialization complete. AWDev is configured to use Codex.\n",
		},
		{
			name:   "retained",
			result: initrepo.Result{Gitignore: initrepo.GitignoreRetained},
			want:   []string{".awdev/ already exists; missing defaults were checked.", ".gitignore already contains the AWDev entries.\n"},
		},
		{
			name:   "gitignore updated",
			result: initrepo.Result{Gitignore: initrepo.GitignoreUpdated},
			want:   []string{".awdev/ already exists; missing defaults were checked.", "Updated .gitignore with awdev state and worktree entries."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services := cli.Services{
				WorkingDirectory: func() (string, error) { return "/repo", nil },
				DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
				Initialize:       func(string, agent.Provider) (initrepo.Result, error) { return test.result, nil },
				ValidateConfig:   func(string) error { return nil },
			}
			var output bytes.Buffer
			command := cli.NewRootCommand(services)
			command.SetIn(strings.NewReader("2\n"))
			command.SetOut(&output)
			command.SetArgs([]string{"init"})

			if err := command.Execute(); err != nil {
				t.Fatalf("execute init: %v", err)
			}
			for _, want := range append(test.want, "Initialization complete. AWDev is configured to use Codex.\n") {
				if !strings.Contains(output.String(), want) {
					t.Errorf("init output %q does not contain %q", output.String(), want)
				}
			}
			if test.wantBlock != "" && !strings.Contains(output.String(), test.wantBlock) {
				t.Errorf("init output %q does not contain exact block %q", output.String(), test.wantBlock)
			}
			for _, unwanted := range []string{".awdev/config.json", "Created:", "Retained:", "Changed:"} {
				if strings.Contains(output.String(), unwanted) {
					t.Errorf("init output %q unexpectedly contains %q", output.String(), unwanted)
				}
			}
		})
	}
}

func TestInitCommandValidatesExistingConfigBeforeInitialization(t *testing.T) {
	t.Setenv("TERM", "dumb")

	initialized := false
	services := cli.Services{
		WorkingDirectory:       func() (string, error) { return "/repo", nil },
		DiscoverRoot:           func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateExistingConfig: func(string) error { return errors.New("invalid existing config") },
		Initialize: func(string, agent.Provider) (initrepo.Result, error) {
			initialized = true
			return initrepo.Result{}, nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetIn(strings.NewReader("2\n"))
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"init"})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid existing config") {
		t.Fatalf("execute error = %v, want preflight config error", err)
	}
	if initialized {
		t.Fatal("repository initialized after existing config failed validation")
	}
}

func TestSourceCommandsValidateArgumentsBeforeServices(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing source", args: []string{"run"}, wantErr: "requires a source and issue number"},
		{name: "legacy run number", args: []string{"run", "17"}, wantErr: "requires a source and issue number"},
		{name: "legacy status number", args: []string{"status", "17"}, wantErr: "requires a source and issue number"},
		{name: "legacy resume number", args: []string{"resume", "17"}, wantErr: "requires a source and issue number"},
		{name: "missing number", args: []string{"resume", "github"}, wantErr: "requires a source and issue number"},
		{name: "unsupported source", args: []string{"retry", "jira", "17"}, wantErr: "unsupported source \"jira\""},
		{name: "linear", args: []string{"run", "linear", "17"}, wantErr: "Linear is not implemented yet."},
		{name: "linear with zero number", args: []string{"run", "linear", "0"}, wantErr: "issue number must be a positive decimal integer"},
		{name: "malformed number", args: []string{"run", "github", "abc"}, wantErr: "issue number must be a positive decimal integer"},
		{name: "zero number", args: []string{"run", "github", "0"}, wantErr: "issue number must be a positive decimal integer"},
		{name: "negative number", args: []string{"run", "github", "-2"}, wantErr: "issue number must be a positive decimal integer"},
		{name: "extra argument", args: []string{"run", "github", "17", "extra"}, wantErr: "accepts 2 arg(s), received 3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			services := cli.Services{
				WorkingDirectory: func() (string, error) { called = true; return "/repo", nil },
				DiscoverRoot:     func(context.Context, string) (string, error) { called = true; return "/repo", nil },
				ValidateConfig:   func(string) error { called = true; return nil },
				Execute:          func(context.Context, cli.Operation, cli.SourceItem, string) error { called = true; return nil },
			}
			command := cli.NewRootCommand(services)
			command.SetOut(new(bytes.Buffer))
			command.SetErr(new(bytes.Buffer))
			command.SetArgs(test.args)

			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("execute error = %v, want substring %q", err, test.wantErr)
			}
			if test.wantErr != "Linear is not implemented yet." {
				operation := test.args[0]
				wantUsage := "Usage: awdev " + operation + " SOURCE NUMBER"
				if !strings.Contains(err.Error(), wantUsage) {
					t.Errorf("execute error %q does not contain source-aware %q", err, wantUsage)
				}
			}
			if called {
				t.Fatal("a service was called before argument validation completed")
			}
		})
	}
}

func TestSourceCommandAcceptsLeadingZeroDecimalIssueNumber(t *testing.T) {
	gotNumber := 0
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		Execute: func(_ context.Context, _ cli.Operation, item cli.SourceItem, _ string) error {
			gotNumber = item.Number
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"run", "github", "017"})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if gotNumber != 17 {
		t.Fatalf("issue number = %d, want 17", gotNumber)
	}
}

func TestSourceCommandDiscoversAndValidatesBeforeExecution(t *testing.T) {
	var events []string
	services := cli.Services{
		WorkingDirectory: func() (string, error) {
			events = append(events, "cwd")
			return "/repo/nested", nil
		},
		DiscoverRoot: func(_ context.Context, cwd string) (string, error) {
			events = append(events, "discover:"+cwd)
			return "/repo", nil
		},
		ValidateConfig: func(root string) error {
			events = append(events, "config:"+root)
			return nil
		},
		Execute: func(_ context.Context, operation cli.Operation, item cli.SourceItem, root string) error {
			events = append(events, string(operation)+":"+string(item.Source)+":"+strconv.Itoa(item.Number)+":"+root)
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"run", "github", "17"})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	want := []string{"cwd", "discover:/repo/nested", "config:/repo", "run:github:17:/repo"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestInvalidConfigStopsBeforeOperation(t *testing.T) {
	operationCalled := false
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return errors.New("bad config") },
		Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error {
			operationCalled = true
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"run", "github", "17"})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("execute error = %v, want configuration error", err)
	}
	if operationCalled {
		t.Fatal("operation ran after invalid configuration")
	}
}

func TestWorkerGuardAppliesOnlyToRunAndResume(t *testing.T) {
	t.Setenv("AWDEV_WORKER", "1")

	for _, operation := range []string{"run", "resume"} {
		t.Run(operation, func(t *testing.T) {
			called := false
			services := cli.Services{WorkingDirectory: func() (string, error) { called = true; return "", nil }}
			command := cli.NewRootCommand(services)
			command.SetOut(new(bytes.Buffer))
			command.SetArgs([]string{operation, "github", "17"})

			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), "cannot be invoked from an awdev worker") {
				t.Fatalf("execute error = %v, want worker guard", err)
			}
			if called {
				t.Fatal("service called despite worker guard")
			}
		})
	}

	statusCalled := false
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		Execute: func(_ context.Context, operation cli.Operation, _ cli.SourceItem, _ string) error {
			statusCalled = operation == cli.OperationStatus
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(new(bytes.Buffer))
	command.SetArgs([]string{"status", "github", "17"})
	if err := command.Execute(); err != nil {
		t.Fatalf("status command: %v", err)
	}
	if !statusCalled {
		t.Fatal("status did not remain available to a worker")
	}
}

func TestHelpShowsSourceAwareForms(t *testing.T) {
	command := cli.NewRootCommand(cli.Services{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"help", "run"})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	for _, want := range []string{"awdev run SOURCE NUMBER", "awdev run github NUMBER"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("help %q does not contain %q", output.String(), want)
		}
	}
}

func TestRunGitHubRendersApplicationServiceResult(t *testing.T) {
	var output bytes.Buffer
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		RunGitHub: func(_ context.Context, root string, number int, _ cli.ProgressReporter) (workflow.RunResult, error) {
			if root != "/repo" || number != 17 {
				t.Fatalf("run service inputs = %q, %d", root, number)
			}
			return workflow.RunResult{
				WorkflowID: cliTestWorkflowID,
				Outcome:    workflow.RunReady,
				Snapshot: githubapi.Snapshot{
					Repository: githubapi.Repository{NameWithOwner: "owner/repository", DefaultBranch: "main"},
					Actor:      githubapi.Actor{Login: "octocat"},
					Issue:      githubapi.Issue{Number: 17},
				},
				Worktree: gitrepo.Worktree{
					Branch:       "gh-17-a-title",
					BaseSHA:      strings.Repeat("a", 40),
					AbsolutePath: "/repo/.awdev/worktrees/gh-17-a-title",
				},
				Manifest: &state.Manifest{
					Worktree:          ".awdev/worktrees/gh-17-a-title",
					SpecificationPath: ".awdev/specs/gh-17-a-title.md",
				},
				Review: &workflow.ReviewResult{Evidence: &reviewapi.Result{Approved: true, Findings: []reviewapi.Finding{}}},
			}, nil
		},
		Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error {
			t.Fatal("generic operation invoked instead of GitHub run service")
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(&output)
	command.SetArgs([]string{"run", "github", "17"})
	if err := command.Execute(); err != nil {
		t.Fatalf("run command: %v", err)
	}
	if got, want := output.String(), ""; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunGitHubRendersExistingWorkflow(t *testing.T) {
	var output bytes.Buffer
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		RunGitHub: func(context.Context, string, int, cli.ProgressReporter) (workflow.RunResult, error) {
			return workflow.RunResult{
				WorkflowID: cliTestWorkflowID,
				Outcome:    workflow.RunExisting,
				Existing:   state.ExistingWorkflow{Exists: true, Manifest: &state.Manifest{Phase: "spec", Status: "blocked"}},
			}, nil
		},
		Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error { return nil },
	}
	command := cli.NewRootCommand(services)
	command.SetOut(&output)
	command.SetArgs([]string{"run", "github", "17"})
	if err := command.Execute(); err != nil {
		t.Fatalf("run command: %v", err)
	}
	if got, want := output.String(), "Workflow "+cliTestWorkflowID+" already exists: spec/blocked.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunGitHubProvidesAVisibleProgressWriterToTheService(t *testing.T) {
	var output bytes.Buffer
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		RunGitHub: func(_ context.Context, _ string, _ int, progress cli.ProgressReporter) (workflow.RunResult, error) {
			want := "agent progress\n"
			progress(cli.ProgressUpdate{Message: "agent progress"})
			if got := output.String(); got != want {
				t.Fatalf("visible progress output = %q, want %q", got, want)
			}
			return workflow.RunResult{
				WorkflowID: cliTestWorkflowID,
				Outcome:    workflow.RunExisting,
				Existing:   state.ExistingWorkflow{Exists: true, Manifest: &state.Manifest{Phase: "spec", Status: "running"}},
			}, nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"run", "github", "17"})
	if err := command.Execute(); err != nil {
		t.Fatalf("run command: %v", err)
	}
}

func TestResumeGitHubRendersWaitingAndContinuedResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		result workflow.ResumeResult
		want   string
	}{
		{
			name: "waiting",
			result: workflow.ResumeResult{Outcome: workflow.ResumeWaiting, Manifest: state.Manifest{
				WorkflowID: cliTestWorkflowID,
				Blocker:    &state.Blocker{Comment: &state.SourceReference{URL: "https://github.com/owner/repository/issues/17#issuecomment-100"}},
			}},
			want: "Workflow " + cliTestWorkflowID + " is still waiting for a reply: https://github.com/owner/repository/issues/17#issuecomment-100\n",
		},
		{
			name: "continued",
			result: workflow.ResumeResult{
				Outcome:  workflow.ResumeContinued,
				Manifest: state.Manifest{WorkflowID: cliTestWorkflowID, Phase: state.PhaseReview, Status: state.StatusRunning},
				Answer:   &state.BlockerAnswer{URL: "https://github.com/owner/repository/issues/17#issuecomment-101"},
				Review:   &workflow.ReviewResult{Evidence: &reviewapi.Result{Approved: true, Findings: []reviewapi.Finding{}}},
			},
			want: "Workflow " + cliTestWorkflowID + " resumed from https://github.com/owner/repository/issues/17#issuecomment-101.\nPhase: review\nStatus: running\nReview: passed\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			services := cli.Services{
				WorkingDirectory: func() (string, error) { return "/repo", nil },
				DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
				ValidateConfig:   func(string) error { return nil },
				ResumeGitHub: func(_ context.Context, root string, number int, progress cli.ProgressReporter) (workflow.ResumeResult, error) {
					if root != "/repo" || number != 17 || progress == nil {
						t.Fatalf("resume inputs = %q, %d, %v", root, number, progress)
					}
					return test.result, nil
				},
				Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error {
					t.Fatal("generic operation invoked instead of GitHub resume service")
					return nil
				},
			}
			command := cli.NewRootCommand(services)
			command.SetOut(&output)
			command.SetArgs([]string{"resume", "github", "17"})
			if err := command.Execute(); err != nil {
				t.Fatalf("resume command: %v", err)
			}
			if output.String() != test.want {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestRetryGitHubRendersCompletedPullRequest(t *testing.T) {
	var output bytes.Buffer
	manifest := state.Manifest{
		WorkflowID: cliTestWorkflowID, Phase: state.PhaseDone, Status: state.StatusDone,
		PullRequest: &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"},
	}
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		RetryGitHub: func(_ context.Context, root string, number int, progress cli.ProgressReporter) (workflow.PublicationResult, error) {
			if root != "/repo" || number != 17 || progress == nil {
				t.Fatalf("retry inputs = %q, %d, %v", root, number, progress)
			}
			return workflow.PublicationResult{Manifest: manifest}, nil
		},
		Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error {
			t.Fatal("generic operation invoked instead of GitHub retry service")
			return nil
		},
	}
	command := cli.NewRootCommand(services)
	command.SetOut(&output)
	command.SetArgs([]string{"retry", "github", "17"})
	if err := command.Execute(); err != nil {
		t.Fatalf("retry command: %v", err)
	}
	want := "Workflow " + cliTestWorkflowID + " completed.\nPull request: https://github.com/owner/repository/pull/23\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestStatusGitHubRendersEveryWorkflowCondition(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*state.Manifest)
		extra string
	}{
		{name: "running", edit: func(manifest *state.Manifest) {}},
		{name: "blocked", edit: func(manifest *state.Manifest) {
			manifest.Phase = state.PhaseSpec
			manifest.Status = state.StatusBlocked
			manifest.Blocker = &state.Blocker{ID: "blocker-1", Phase: state.PhaseSpec, Question: "Which behavior?", Comment: &state.SourceReference{ID: "123", URL: "https://github.com/owner/repository/issues/17#issuecomment-123"}}
		}, extra: "Blocker: Which behavior?\n"},
		{name: "failed", edit: func(manifest *state.Manifest) {
			manifest.Phase = state.PhaseImplementation
			manifest.Status = state.StatusFailed
			manifest.LastError = &state.WorkflowError{Code: "technical_failure", Message: "safe\n \x1b[31mdiagnostic\x1b[0m\ttext"}
		}, extra: "Last error: safe diagnostic text\n"},
		{name: "done", edit: func(manifest *state.Manifest) {
			manifest.Phase = state.PhaseDone
			manifest.Status = state.StatusDone
			manifest.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
		}, extra: "Pull request: https://github.com/owner/repository/pull/23\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := cliStatusManifest()
			test.edit(&manifest)
			var output bytes.Buffer
			services := statusServices(func(_ context.Context, root string, number int, options workflow.StatusOptions) (workflow.StatusResult, error) {
				if root != "/repo" || number != 17 || options.CheckIssue {
					t.Fatalf("status inputs = %q, %d, %#v", root, number, options)
				}
				return workflow.StatusResult{Manifest: manifest}, nil
			})
			command := cli.NewRootCommand(services)
			command.SetOut(&output)
			command.SetArgs([]string{"status", "github", "17"})
			if err := command.Execute(); err != nil {
				t.Fatalf("status command: %v", err)
			}
			want := "Workflow: " + cliTestWorkflowID + "\nSource: github\nIssue: owner/repository#17 — A title\nPhase: " + string(manifest.Phase) + "\nStatus: " + string(manifest.Status) + "\n" + test.extra
			if output.String() != want {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestStatusGitHubRendersStableJSONAndDriftWarning(t *testing.T) {
	manifest := cliStatusManifest()
	manifest.Phase = state.PhaseSpec
	manifest.Status = state.StatusBlocked
	manifest.Blocker = &state.Blocker{ID: "blocker-1", Phase: state.PhaseSpec, Question: "Which behavior?", Comment: &state.SourceReference{ID: "123", URL: "https://github.com/owner/repository/issues/17#issuecomment-123"}}
	var output bytes.Buffer
	services := statusServices(func(_ context.Context, _ string, _ int, options workflow.StatusOptions) (workflow.StatusResult, error) {
		if !options.CheckIssue {
			t.Fatal("--check-issue was not forwarded")
		}
		return workflow.StatusResult{Manifest: manifest, Warning: "GitHub issue changed; using the saved snapshot."}, nil
	})
	command := cli.NewRootCommand(services)
	command.SetOut(&output)
	command.SetArgs([]string{"status", "github", "17", "--json", "--check-issue"})
	if err := command.Execute(); err != nil {
		t.Fatalf("status command: %v", err)
	}
	want := "{\n" +
		"  \"workflow_id\": \"" + cliTestWorkflowID + "\",\n" +
		"  \"source\": \"github\",\n" +
		"  \"repository\": \"owner/repository\",\n" +
		"  \"issue\": {\n" +
		"    \"number\": 17,\n" +
		"    \"title\": \"A title\",\n" +
		"    \"url\": \"https://github.com/owner/repository/issues/17\"\n" +
		"  },\n" +
		"  \"phase\": \"spec\",\n" +
		"  \"status\": \"blocked\",\n" +
		"  \"blocker_question\": \"Which behavior?\",\n" +
		"  \"warning\": \"GitHub issue changed; using the saved snapshot.\"\n" +
		"}\n"
	if output.String() != want {
		t.Fatalf("JSON output = %q, want %q", output.String(), want)
	}
}

func TestStatusGitHubStableJSONGoldensForRunningFailedAndDone(t *testing.T) {
	tests := []struct {
		name string
		edit func(*state.Manifest)
		want string
	}{
		{name: "running", edit: func(*state.Manifest) {}, want: `{
  "workflow_id": "wf_0123456789abcdef0123456789abcdef",
  "source": "github",
  "repository": "owner/repository",
  "issue": {
    "number": 17,
    "title": "A title",
    "url": "https://github.com/owner/repository/issues/17"
  },
  "phase": "init",
  "status": "running"
}
`},
		{name: "failed", edit: func(manifest *state.Manifest) {
			manifest.Phase = state.PhaseImplementation
			manifest.Status = state.StatusFailed
			manifest.LastError = &state.WorkflowError{Code: "technical_failure", Message: "safe\n\x1b[31mdiagnostic\x1b[0m\ttext"}
		}, want: `{
  "workflow_id": "wf_0123456789abcdef0123456789abcdef",
  "source": "github",
  "repository": "owner/repository",
  "issue": {
    "number": 17,
    "title": "A title",
    "url": "https://github.com/owner/repository/issues/17"
  },
  "phase": "implementation",
  "status": "failed",
  "last_error": "safe\n\u001b[31mdiagnostic\u001b[0m\ttext"
}
`},
		{name: "done", edit: func(manifest *state.Manifest) {
			manifest.Phase = state.PhaseDone
			manifest.Status = state.StatusDone
			manifest.PullRequest = &state.PullRequest{Number: 23, URL: "https://github.com/owner/repository/pull/23"}
		}, want: `{
  "workflow_id": "wf_0123456789abcdef0123456789abcdef",
  "source": "github",
  "repository": "owner/repository",
  "issue": {
    "number": 17,
    "title": "A title",
    "url": "https://github.com/owner/repository/issues/17"
  },
  "phase": "done",
  "status": "done",
  "pull_request_url": "https://github.com/owner/repository/pull/23"
}
`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := cliStatusManifest()
			test.edit(&manifest)
			var output bytes.Buffer
			command := cli.NewRootCommand(statusServices(func(context.Context, string, int, workflow.StatusOptions) (workflow.StatusResult, error) {
				return workflow.StatusResult{Manifest: manifest}, nil
			}))
			command.SetOut(&output)
			command.SetArgs([]string{"status", "github", "17", "--json"})
			if err := command.Execute(); err != nil {
				t.Fatalf("status command: %v", err)
			}
			if output.String() != test.want {
				t.Fatalf("JSON output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func statusServices(status func(context.Context, string, int, workflow.StatusOptions) (workflow.StatusResult, error)) cli.Services {
	return cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		StatusGitHub:     status,
		Execute: func(context.Context, cli.Operation, cli.SourceItem, string) error {
			return errors.New("generic operation should not run")
		},
	}
}

func cliStatusManifest() state.Manifest {
	return state.Manifest{
		SchemaVersion: state.CurrentSchemaVersion,
		WorkflowID:    cliTestWorkflowID,
		Source:        state.SourceGitHub,
		Repository:    "owner/repository",
		DefaultBranch: "main",
		Issue:         state.IssueSnapshot{Number: 17, Title: "A title", Body: "saved body", URL: "https://github.com/owner/repository/issues/17", State: "OPEN", UpdatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)},
		Actor:         "octocat",
		Phase:         state.PhaseInit,
		Status:        state.StatusRunning,
		Branch:        "gh-17-a-title",
		BaseSHA:       strings.Repeat("a", 40),
		Worktree:      ".awdev/worktrees/gh-17-a-title",
	}
}
