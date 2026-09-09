package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var (
	repositoryRoot string
	awdevBinary    string
	fakeBinary     string
)

type traceEvent struct {
	Phase              string   `json:"phase"`
	Tool               string   `json:"tool"`
	Args               []string `json:"args"`
	Directory          string   `json:"directory"`
	Stdin              string   `json:"stdin"`
	PromptDisabled     string   `json:"gh_prompt_disabled"`
	NoColor            string   `json:"no_color"`
	Worker             string   `json:"awdev_worker"`
	ManifestPresent    bool     `json:"manifest_present"`
	WorktreeRegistered bool     `json:"worktree_registered"`
	Stdout             string   `json:"stdout"`
	Stderr             string   `json:"stderr"`
	ExitCode           *int     `json:"exit_code"`
}

type fixture struct {
	root     string
	fakeDir  string
	checkBin string
	provider agentProvider
}

// agentProvider names the agent adapter under test: the config section written
// into .awdev/config.json and the fake binary linked into .fake/bin.
type agentProvider struct {
	config string
	tool   string
}

var (
	codexProvider  = agentProvider{config: "codex", tool: "codex"}
	claudeProvider = agentProvider{config: "claude-code", tool: "claude"}
	allProviders   = []agentProvider{codexProvider, claudeProvider}
)

// retargetTrace rewrites the "codex:" trace labels in a want list to the label
// prefix the given provider emits.
func retargetTrace(labels []string, tool string) []string {
	if tool == "codex" {
		return labels
	}
	out := make([]string, len(labels))
	for index, label := range labels {
		out[index] = strings.Replace(label, "codex:", tool+":", 1)
	}
	return out
}

type stableStatus struct {
	WorkflowID      string `json:"workflow_id"`
	Phase           string `json:"phase"`
	Status          string `json:"status"`
	BlockerQuestion string `json:"blocker_question"`
	LastError       string `json:"last_error"`
	PullRequestURL  string `json:"pull_request_url"`
}

type scenarioMode string

const (
	scenarioHappy         scenarioMode = "happy"
	scenarioCheckFix      scenarioMode = "check-fix"
	scenarioCheckExhaust  scenarioMode = "check-exhaust"
	scenarioReviewFix     scenarioMode = "review-fix"
	scenarioReviewExhaust scenarioMode = "review-exhaust"
	scenarioBlocker       scenarioMode = "blocker"
	scenarioPRCreateFail  scenarioMode = "pr-create-error"
	scenarioCancellation  scenarioMode = "cancellation"
)

func TestMain(m *testing.M) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "locate e2e package")
		os.Exit(1)
	}
	repositoryRoot = filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	buildDirectory, err := os.MkdirTemp("", "awdev-e2e-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	awdevBinary = filepath.Join(buildDirectory, executableName("awdev"))
	fakeBinary = filepath.Join(buildDirectory, executableName("fake-tool"))
	awdevBuild := exec.Command("go", "build", "-o", awdevBinary, "./cmd/awdev")
	awdevBuild.Dir = repositoryRoot
	if output, buildErr := awdevBuild.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "build awdev: %v\n%s", buildErr, output)
		os.Exit(1)
	}
	fakeBuild := exec.Command("go", "build", "-o", fakeBinary, "./internal/e2e/testdata/fake-tool")
	fakeBuild.Dir = repositoryRoot
	if output, buildErr := fakeBuild.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "build fake tool: %v\n%s", buildErr, output)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(buildDirectory)
	os.Exit(code)
}

func TestRootHelpAndInitializationAreUsableAsBuilt(t *testing.T) {
	fixture := newFixture(t, scenarioHappy)
	output := fixture.run(t)
	for _, expected := range []string{"Agentic Workflow Development CLI", "awdev [command]", "init", "run"} {
		if !strings.Contains(output, expected) {
			t.Errorf("root help %q does not contain %q", output, expected)
		}
	}

	output = fixture.runWithInput(t, "2\n", "init")
	if !strings.Contains(output, "Initialization complete. AWDev is configured to use Codex.") {
		t.Fatalf("init output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, ".awdev", "config.json")); err != nil {
		t.Fatalf("initialized config: %v", err)
	}
}

func TestHappyAndCorrectionJourneysAsBuilt(t *testing.T) {
	tests := []struct {
		name            string
		mode            scenarioMode
		wantPromptOrder []string
		wantTrace       []string
	}{
		{
			name: "happy", mode: scenarioHappy,
			wantPromptOrder: []string{"Create an implementation specification", "Implement the supplied specification", "Review the current worktree"},
			wantTrace:       []string{"gh:repo", "gh:actor", "gh:issue", "git:worktree-add", "git:worktree-validate", "codex:spec", "codex:implementation", "check", "codex:review", "check", "git:commit", "git:push", "gh:pr-list", "gh:pr-create"},
		},
		{
			name: "check correction", mode: scenarioCheckFix,
			wantPromptOrder: []string{"Create an implementation specification", "Implement the supplied specification", "Repair the implementation", "Review the current worktree"},
			wantTrace:       []string{"gh:repo", "gh:actor", "gh:issue", "git:worktree-add", "git:worktree-validate", "codex:spec", "codex:implementation", "check", "codex:check-repair", "check", "codex:review", "check", "git:commit", "git:push", "gh:pr-list", "gh:pr-create"},
		},
		{
			name: "review correction", mode: scenarioReviewFix,
			wantPromptOrder: []string{"Create an implementation specification", "Implement the supplied specification", "Review the current worktree", "Correct the implementation", "Review the current worktree"},
			wantTrace:       []string{"gh:repo", "gh:actor", "gh:issue", "git:worktree-add", "git:worktree-validate", "codex:spec", "codex:implementation", "check", "codex:review", "codex:review-correction", "check", "codex:review", "check", "git:commit", "git:push", "gh:pr-list", "gh:pr-create"},
		},
	}

	for _, provider := range allProviders {
		for _, test := range tests {
			t.Run(provider.config+"/"+test.name, func(t *testing.T) {
				wantTrace := retargetTrace(test.wantTrace, provider.tool)
				fixture := newFixtureForProvider(t, test.mode, provider)
				fixture.initialize(t)
				output := fixture.run(t, "run", "github", "123")
				if !strings.Contains(output, "Pull request created: https://github.com/owner/repo/pull/77") {
					t.Fatalf("run output = %q", output)
				}

				stable := fixture.status(t)
				if !strings.HasPrefix(stable.WorkflowID, "wf_") || stable.Phase != "done" || stable.Status != "done" || stable.PullRequestURL != "https://github.com/owner/repo/pull/77" {
					t.Fatalf("stable status = %#v", stable)
				}

				events := fixture.trace(t)
				assertGitHubBootstrap(t, events, provider.tool)
				if got := journeyTrace(events); !equalTrace(got, wantTrace) {
					t.Fatalf("journey trace =\n%q\nwant\n%q", got, wantTrace)
				}
				if got := prompts(events); !promptsMatchPrefixes(got, test.wantPromptOrder) {
					t.Fatalf("agent prompt order = %q, want %q", got, test.wantPromptOrder)
				}
				assertSafeExternalInvocations(t, events, provider.tool)

				before := len(events)
				duplicate := fixture.run(t, "run", "github", "123")
				if !strings.Contains(duplicate, "already exists: done/done") {
					t.Fatalf("duplicate run output = %q", duplicate)
				}
				for _, event := range fixture.trace(t)[before:] {
					if event.Tool == "gh" || event.Tool == provider.tool || (event.Tool == "git" && strings.HasPrefix(strings.Join(event.Args, " "), "worktree add")) {
						t.Fatalf("duplicate run repeated a workflow side effect: %#v", event)
					}
				}
			})
		}
	}
}

func TestHumanBlockerResumesThroughOneMarkedComment(t *testing.T) {
	for _, provider := range allProviders {
		t.Run(provider.config, func(t *testing.T) {
			humanBlockerResumesThroughOneMarkedComment(t, provider)
		})
	}
}

func humanBlockerResumesThroughOneMarkedComment(t *testing.T, provider agentProvider) {
	fixture := newFixtureForProvider(t, scenarioBlocker, provider)
	fixture.initialize(t)
	first := fixture.run(t, "run", "github", "123")
	if !strings.Contains(first, "Waiting for reply: https://github.com/owner/repo/issues/123#issuecomment-100") {
		t.Fatalf("blocked run output = %q", first)
	}
	blockedStatus := fixture.status(t)
	if blockedStatus.Phase != "spec" || blockedStatus.Status != "blocked" || blockedStatus.BlockerQuestion != "Which compatibility behavior should be used?" {
		t.Fatalf("blocked JSON status = %#v", blockedStatus)
	}

	commentsPath := filepath.Join(fixture.root, ".fake", "comments.json")
	var comments []map[string]any
	readJSON(t, commentsPath, &comments)
	if len(comments) != 1 || !strings.Contains(comments[0]["body"].(string), "<!-- awdev:blocker workflow=wf_") || !strings.Contains(comments[0]["body"].(string), " id=blocker-1 -->") {
		t.Fatalf("published comments = %#v", comments)
	}
	duplicate := fixture.run(t, "run", "github", "123")
	if !strings.Contains(duplicate, "already exists: spec/blocked") {
		t.Fatalf("duplicate blocked run output = %q", duplicate)
	}
	readJSON(t, commentsPath, &comments)
	if len(comments) != 1 {
		t.Fatalf("duplicate blocked run published %d comments, want 1", len(comments))
	}
	comments = append(comments, map[string]any{
		"id": 101, "html_url": "https://github.com/owner/repo/issues/123#issuecomment-101", "body": "Keep backward compatibility.",
		"created_at": "2026-09-01T12:02:00Z", "user": map[string]any{"login": "maintainer", "type": "User"},
	})
	writeJSON(t, commentsPath, comments)
	writeJSON(t, filepath.Join(fixture.root, ".fake", "scenario.json"), map[string]scenarioMode{"mode": scenarioHappy})

	resumed := fixture.run(t, "resume", "github", "123")
	if !strings.Contains(resumed, "resumed from https://github.com/owner/repo/issues/123#issuecomment-101") || !strings.Contains(resumed, "Pull request: https://github.com/owner/repo/pull/77") {
		t.Fatalf("resume output = %q", resumed)
	}
	if status := fixture.status(t); status.Phase != "done" || status.Status != "done" || status.PullRequestURL == "" {
		t.Fatalf("resumed JSON status = %#v", status)
	}
	events := fixture.trace(t)
	commentPosts := 0
	for _, event := range events {
		if event.Tool == "gh" && len(event.Args) >= 2 && event.Args[0] == "issue" && event.Args[1] == "comment" {
			commentPosts++
		}
	}
	if commentPosts != 1 {
		t.Fatalf("blocker comment posts = %d, want 1", commentPosts)
	}
	want := []string{"Create an implementation specification", "Continue the recorded workflow phase", "Implement the supplied specification", "Review the current worktree"}
	if got := prompts(events); !promptsMatchPrefixes(got, want) {
		t.Fatalf("agent prompt order = %q, want %q", got, want)
	}
}

func TestRetryReconcilesPullRequestCreatedBeforeLocalFailure(t *testing.T) {
	fixture := newFixture(t, scenarioPRCreateFail)
	fixture.initialize(t)
	failed := fixture.runExpectError(t, "run", "github", "123")
	if !strings.Contains(failed, "simulated connection loss after PR creation") {
		t.Fatalf("failed publication output = %q", failed)
	}
	if status := fixture.status(t); status.Phase != "pull_request" || status.Status != "failed" || !strings.Contains(status.LastError, "simulated connection loss") {
		t.Fatalf("failed publication JSON status = %#v", status)
	}
	writeJSON(t, filepath.Join(fixture.root, ".fake", "scenario.json"), map[string]scenarioMode{"mode": scenarioHappy})
	retried := fixture.run(t, "retry", "github", "123")
	if !strings.Contains(retried, "Pull request: https://github.com/owner/repo/pull/77") {
		t.Fatalf("retry output = %q", retried)
	}
	if status := fixture.status(t); status.Phase != "done" || status.Status != "done" || status.PullRequestURL == "" || status.LastError != "" {
		t.Fatalf("reconciled JSON status = %#v", status)
	}
	creates := 0
	for _, event := range fixture.trace(t) {
		if event.Tool == "gh" && len(event.Args) >= 2 && event.Args[0] == "pr" && event.Args[1] == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("pull request creates = %d, want one reconciled create", creates)
	}
}

func TestCorrectionLoopsStopAtTheirDocumentedBounds(t *testing.T) {
	for _, provider := range allProviders {
		t.Run(provider.config, func(t *testing.T) {
			correctionLoopsStopAtTheirDocumentedBounds(t, provider)
		})
	}
}

func correctionLoopsStopAtTheirDocumentedBounds(t *testing.T, provider agentProvider) {
	t.Run("check repairs", func(t *testing.T) {
		fixture := newFixtureForProvider(t, scenarioCheckExhaust, provider)
		fixture.initialize(t)
		output := fixture.runExpectError(t, "run", "github", "123")
		if !strings.Contains(output, "still failed after 3 repair attempts") {
			t.Fatalf("exhausted check output = %q", output)
		}
		status := fixture.status(t)
		if status.Phase != "implementation" || status.Status != "failed" || !strings.Contains(status.LastError, "3 repair attempts") {
			t.Fatalf("exhausted check JSON status = %#v", status)
		}
		if countPromptPrefix(fixture.trace(t), "Repair the implementation") != 3 {
			t.Fatal("check correction did not stop after three repair invocations")
		}
	})

	t.Run("review attempts", func(t *testing.T) {
		fixture := newFixtureForProvider(t, scenarioReviewExhaust, provider)
		fixture.initialize(t)
		output := fixture.run(t, "run", "github", "123")
		if !strings.Contains(output, "Waiting for reply:") {
			t.Fatalf("exhausted review output = %q", output)
		}
		status := fixture.status(t)
		if status.Phase != "review" || status.Status != "blocked" || status.BlockerQuestion == "" {
			t.Fatalf("exhausted review JSON status = %#v", status)
		}
		events := fixture.trace(t)
		if countPromptPrefix(events, "Review the current worktree") != 3 || countPromptPrefix(events, "Correct the implementation") != 2 {
			t.Fatalf("review loop counts: reviews=%d corrections=%d", countPromptPrefix(events, "Review the current worktree"), countPromptPrefix(events, "Correct the implementation"))
		}
	})
}

func newFixture(t *testing.T, mode scenarioMode) fixture {
	t.Helper()
	return newFixtureForProvider(t, mode, codexProvider)
}

func newFixtureForProvider(t *testing.T, mode scenarioMode, provider agentProvider) fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "controller")
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runCommand(t, root, "git", "init", "-b", "main")
	runCommand(t, root, "git", "config", "user.name", "AWDev Test")
	runCommand(t, root, "git", "config", "user.email", "awdev@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommand(t, root, "git", "add", "README.md")
	runCommand(t, root, "git", "commit", "-m", "initial")
	runCommand(t, filepath.Dir(origin), "git", "init", "--bare", origin)
	// Keep the public remote identity realistic while redirecting transport to
	// the local bare fixture repository.
	runCommand(t, root, "git", "config", "url."+origin+".insteadOf", "https://github.com/owner/repo.git")
	runCommand(t, root, "git", "remote", "add", "origin", "https://github.com/owner/repo.git")
	runCommand(t, root, "git", "push", "-u", "origin", "main")

	fakeDir := filepath.Join(root, ".fake", "bin")
	if err := os.MkdirAll(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".fake", "real-git-path"), []byte(realGit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", provider.tool, "awdev-check", "git"} {
		copyExecutable(t, fakeBinary, filepath.Join(fakeDir, executableName(name)))
	}
	writeJSON(t, filepath.Join(root, ".fake", "scenario.json"), map[string]scenarioMode{"mode": mode})
	return fixture{root: root, fakeDir: fakeDir, checkBin: filepath.Join(fakeDir, executableName("awdev-check")), provider: provider}
}

func (fixture fixture) initialize(t *testing.T) {
	t.Helper()
	fixture.runWithInput(t, "2\n", "init")
	configuration := map[string]any{
		"schema_version":  1,
		"agent":           map[string]any{"provider": fixture.provider.config, "timeout": "30s"},
		"checks":          []any{map[string]any{"name": "fixture", "command": []string{fixture.checkBin}, "timeout": "10s"}},
		"review":          map[string]any{"max_attempts": 3},
		"protected_paths": []string{},
	}
	agentBinary := filepath.Join(fixture.fakeDir, executableName(fixture.provider.tool))
	switch fixture.provider.tool {
	case "claude":
		configuration["claude_code"] = map[string]any{"binary": agentBinary}
	default:
		configuration["codex"] = map[string]any{"binary": agentBinary}
	}
	writeJSON(t, filepath.Join(fixture.root, ".awdev", "config.json"), configuration)
}

func (fixture fixture) run(t *testing.T, args ...string) string {
	t.Helper()
	return fixture.runWithInput(t, "", args...)
}

func (fixture fixture) runWithInput(t *testing.T, input string, args ...string) string {
	t.Helper()
	command := fixture.command(args...)
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("awdev %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (fixture fixture) runExpectError(t *testing.T, args ...string) string {
	t.Helper()
	command := fixture.command(args...)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("awdev %s unexpectedly succeeded\n%s", strings.Join(args, " "), output)
	}
	return string(output)
}

func (fixture fixture) status(t *testing.T) stableStatus {
	t.Helper()
	output := fixture.run(t, "status", "github", "123", "--json")
	var status stableStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		t.Fatalf("decode status JSON %q: %v", output, err)
	}
	return status
}

func (fixture fixture) command(args ...string) *exec.Cmd {
	command := exec.Command(awdevBinary, args...)
	command.Dir = fixture.root
	command.Env = append(os.Environ(), "PATH="+fixture.fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TERM=dumb", "CODEX_HOME="+filepath.Join(fixture.root, ".fake", "codex-home"))
	return command
}

func (fixture fixture) trace(t *testing.T) []traceEvent {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(fixture.root, ".fake", "trace.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var events []traceEvent
	for decoder.More() {
		var event traceEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func assertGitHubBootstrap(t *testing.T, events []traceEvent, agentTool string) {
	t.Helper()
	if len(events) < 5 {
		t.Fatalf("external trace too short: %#v", events)
	}
	issueIndex, worktreeIndex, validationIndex, agentIndex := -1, -1, -1, -1
	for index, event := range events {
		if event.Phase != "start" {
			continue
		}
		joined := strings.Join(event.Args, " ")
		if event.Tool == "gh" && strings.HasPrefix(joined, "issue view") {
			issueIndex = index
		}
		if event.Tool == "git" && strings.HasPrefix(joined, "worktree add") {
			worktreeIndex = index
		}
		if event.Tool == "git" && worktreeIndex >= 0 && index > worktreeIndex && joined == "worktree list --porcelain" {
			validationIndex = index
		}
		if event.Tool == agentTool && agentIndex < 0 {
			agentIndex = index
		}
	}
	if issueIndex < 0 || worktreeIndex <= issueIndex || validationIndex <= worktreeIndex || agentIndex <= validationIndex {
		t.Fatalf("bootstrap order issue=%d worktree=%d validation=%d agent=%d", issueIndex, worktreeIndex, validationIndex, agentIndex)
	}
	if events[validationIndex].ManifestPresent {
		t.Fatalf("initial manifest existed before worktree validation: %#v", events[validationIndex])
	}
	firstAgent := events[agentIndex]
	if !strings.HasPrefix(firstAgent.Stdin, "Create an implementation specification") || !firstAgent.ManifestPresent || !firstAgent.WorktreeRegistered {
		t.Fatalf("first specification event lacks durable validated bootstrap: %#v", firstAgent)
	}
}

func assertSafeExternalInvocations(t *testing.T, events []traceEvent, agentTool string) {
	t.Helper()
	starts := map[string]int{}
	completions := map[string]int{}
	capturedOutput := map[string]bool{}
	for _, event := range events {
		if event.Phase == "complete" && (event.Tool == "gh" || event.Tool == agentTool) {
			completions[event.Tool]++
			capturedOutput[event.Tool] = capturedOutput[event.Tool] || event.Stdout != "" || event.Stderr != ""
			if event.ExitCode == nil {
				t.Errorf("%s completion omitted its exit code", event.Tool)
			}
			continue
		}
		if event.Phase != "start" {
			continue
		}
		joined := strings.Join(event.Args, " ")
		switch event.Tool {
		case "gh":
			starts[event.Tool]++
			if event.PromptDisabled != "1" || event.NoColor != "1" {
				t.Errorf("gh environment is interactive: %#v", event)
			}
		case "codex":
			starts[event.Tool]++
			for _, required := range []string{"exec", "--json", "--sandbox", "--output-schema", "--cd", "--output-last-message"} {
				if !strings.Contains(joined, required) {
					t.Errorf("codex argv %q lacks %q", joined, required)
				}
			}
			if event.Worker != "1" || event.Args[len(event.Args)-1] != "-" {
				t.Errorf("codex invocation is not a guarded stdin invocation: %#v", event)
			}
			if strings.Contains(joined, "--danger") || strings.Contains(joined, "{{.WorkflowID}}") {
				t.Errorf("issue data escaped into argv: %q", joined)
			}
		case "claude":
			starts[event.Tool]++
			for _, required := range []string{"--print", "--output-format stream-json", "--verbose", "--json-schema", "--permission-mode", "--permission-prompts none", "--session-id", "--add-dir"} {
				if !strings.Contains(joined, required) {
					t.Errorf("claude argv %q lacks %q", joined, required)
				}
			}
			if event.Worker != "1" {
				t.Errorf("claude invocation missing AWDEV_WORKER guard: %#v", event)
			}
			if event.Stdin == "" || strings.HasSuffix(joined, " -") {
				t.Errorf("claude prompt is not a guarded stdin invocation: %#v", event)
			}
			if strings.Contains(joined, "--danger") || strings.Contains(joined, "{{.WorkflowID}}") {
				t.Errorf("issue data escaped into argv: %q", joined)
			}
		}
	}
	for _, tool := range []string{"gh", agentTool} {
		if starts[tool] != completions[tool] || !capturedOutput[tool] {
			t.Errorf("%s trace starts=%d completions=%d captured_output=%t", tool, starts[tool], completions[tool], capturedOutput[tool])
		}
	}
}

func isAgentTool(tool string) bool {
	return tool == "codex" || tool == "claude"
}

func prompts(events []traceEvent) []string {
	var result []string
	for _, event := range events {
		if event.Phase != "start" || !isAgentTool(event.Tool) {
			continue
		}
		line, _, _ := strings.Cut(event.Stdin, "\n")
		result = append(result, line)
	}
	return result
}

func countPromptPrefix(events []traceEvent, prefix string) int {
	count := 0
	for _, prompt := range prompts(events) {
		if strings.HasPrefix(prompt, prefix) {
			count++
		}
	}
	return count
}

func journeyTrace(events []traceEvent) []string {
	labels := make([]string, 0)
	worktreeAdded := false
	for _, event := range events {
		if event.Phase != "start" {
			continue
		}
		joined := strings.Join(event.Args, " ")
		switch {
		case event.Tool == "gh" && strings.HasPrefix(joined, "repo view"):
			labels = append(labels, "gh:repo")
		case event.Tool == "gh" && strings.HasPrefix(joined, "api graphql"):
			labels = append(labels, "gh:actor")
		case event.Tool == "gh" && strings.HasPrefix(joined, "issue view"):
			labels = append(labels, "gh:issue")
		case event.Tool == "git" && strings.HasPrefix(joined, "worktree add"):
			worktreeAdded = true
			labels = append(labels, "git:worktree-add")
		case event.Tool == "git" && worktreeAdded && joined == "worktree list --porcelain":
			labels = append(labels, "git:worktree-validate")
		case isAgentTool(event.Tool):
			labels = append(labels, agentTraceLabel(event.Tool, event.Stdin))
		case event.Tool == "awdev-check":
			labels = append(labels, "check")
		case event.Tool == "git" && strings.HasPrefix(joined, "commit --no-gpg-sign"):
			labels = append(labels, "git:commit")
		case event.Tool == "git" && strings.HasPrefix(joined, "push --porcelain"):
			labels = append(labels, "git:push")
		case event.Tool == "gh" && strings.HasPrefix(joined, "pr list"):
			labels = append(labels, "gh:pr-list")
		case event.Tool == "gh" && strings.HasPrefix(joined, "pr create"):
			labels = append(labels, "gh:pr-create")
		}
	}
	return labels
}

func agentTraceLabel(tool, prompt string) string {
	switch {
	case strings.HasPrefix(prompt, "Create an implementation specification"):
		return tool + ":spec"
	case strings.HasPrefix(prompt, "Implement the supplied specification"):
		return tool + ":implementation"
	case strings.HasPrefix(prompt, "Repair the implementation"):
		return tool + ":check-repair"
	case strings.HasPrefix(prompt, "Review the current worktree"):
		return tool + ":review"
	case strings.HasPrefix(prompt, "Correct the implementation"):
		return tool + ":review-correction"
	case strings.HasPrefix(prompt, "Continue the recorded workflow phase"):
		return tool + ":resume"
	default:
		return tool + ":unknown"
	}
}

func equalTrace(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func runCommand(t *testing.T, directory, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

func copyExecutable(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, destination any) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, destination); err != nil {
		t.Fatal(err)
	}
}

func promptsMatchPrefixes(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if !strings.HasPrefix(actual[index], expected[index]) {
			return false
		}
	}
	return true
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
