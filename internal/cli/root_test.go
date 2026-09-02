package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/cli"
	githubapi "github.com/agenticworkflowdev/cli/internal/github"
	"github.com/agenticworkflowdev/cli/internal/initrepo"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

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
		{name: "Codex", input: "2\n", wantAgent: "Codex", wantOutput: "Initialization complete. awdev is configured to use Codex.\n"},
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
		name   string
		result initrepo.Result
		want   []string
	}{
		{
			name:   "created",
			result: initrepo.Result{AwdevDirectoryCreated: true, Gitignore: initrepo.GitignoreCreated},
			want:   []string{"Created .awdev/.", "Created .gitignore with awdev state and worktree entries."},
		},
		{
			name:   "retained",
			result: initrepo.Result{Gitignore: initrepo.GitignoreRetained},
			want:   []string{".awdev/ already exists; missing defaults were checked.", ".gitignore already contains the awdev entries."},
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
			for _, want := range append(test.want, "Initialization complete. awdev is configured to use Codex.") {
				if !strings.Contains(output.String(), want) {
					t.Errorf("init output %q does not contain %q", output.String(), want)
				}
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
		RunGitHub: func(_ context.Context, root string, number int) (workflow.RunResult, error) {
			if root != "/repo" || number != 17 {
				t.Fatalf("run service inputs = %q, %d", root, number)
			}
			return workflow.RunResult{
				WorkflowID: "gh-17",
				Outcome:    workflow.RunReady,
				Snapshot: githubapi.Snapshot{
					Repository: githubapi.Repository{NameWithOwner: "owner/repository", DefaultBranch: "main"},
					Actor:      githubapi.Actor{Login: "octocat"},
					Issue:      githubapi.Issue{Number: 17},
				},
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
	if got, want := output.String(), "Validated GitHub issue owner/repository#17 on main as octocat.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunGitHubRendersExistingWorkflow(t *testing.T) {
	var output bytes.Buffer
	services := cli.Services{
		WorkingDirectory: func() (string, error) { return "/repo", nil },
		DiscoverRoot:     func(context.Context, string) (string, error) { return "/repo", nil },
		ValidateConfig:   func(string) error { return nil },
		RunGitHub: func(context.Context, string, int) (workflow.RunResult, error) {
			return workflow.RunResult{
				WorkflowID: "gh-17",
				Outcome:    workflow.RunExisting,
				Existing:   state.ExistingWorkflow{Exists: true, Phase: "spec", Status: "blocked"},
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
	if got, want := output.String(), "Workflow gh-17 already exists: spec/blocked.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
