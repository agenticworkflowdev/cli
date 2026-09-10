package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type scenario struct {
	Mode scenarioMode `json:"mode"`
}

type scenarioMode string

const (
	scenarioBlocker       scenarioMode = "blocker"
	scenarioCheckFix      scenarioMode = "check-fix"
	scenarioCheckExhaust  scenarioMode = "check-exhaust"
	scenarioReviewFix     scenarioMode = "review-fix"
	scenarioReviewExhaust scenarioMode = "review-exhaust"
	scenarioPRCreateFail  scenarioMode = "pr-create-error"
	scenarioCancellation  scenarioMode = "cancellation"
)

type traceEvent struct {
	Phase              string   `json:"phase"`
	Tool               string   `json:"tool"`
	Args               []string `json:"args"`
	Directory          string   `json:"directory"`
	Stdin              string   `json:"stdin,omitempty"`
	PromptDisabled     string   `json:"gh_prompt_disabled,omitempty"`
	NoColor            string   `json:"no_color,omitempty"`
	Worker             string   `json:"awdev_worker,omitempty"`
	ManifestPresent    bool     `json:"manifest_present,omitempty"`
	WorktreeRegistered bool     `json:"worktree_registered,omitempty"`
	Stdout             string   `json:"stdout,omitempty"`
	Stderr             string   `json:"stderr,omitempty"`
	ExitCode           *int     `json:"exit_code,omitempty"`
}

type toolResult struct {
	stdout string
	stderr string
	exit   int
}

type comment struct {
	ID        int    `json:"id"`
	HTMLURL   string `json:"html_url"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      user   `json:"user"`
}

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "descendant" {
		for {
			time.Sleep(time.Hour)
		}
	}
	tool := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	root, err := findRoot()
	if err != nil {
		fatal(err)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(err)
	}
	event := traceEvent{
		Phase: "start", Tool: tool, Args: append([]string(nil), os.Args[1:]...), Directory: mustGetwd(), Stdin: string(input),
		PromptDisabled: os.Getenv("GH_PROMPT_DISABLED"), NoColor: os.Getenv("NO_COLOR"), Worker: os.Getenv("AWDEV_WORKER"),
		ManifestPresent: oneManifestExists(root),
	}
	if tool == "codex" || tool == "claude" {
		info, statErr := os.Stat(filepath.Join(event.Directory, ".git"))
		event.WorktreeRegistered = statErr == nil && info.Mode().IsRegular()
	}
	if err := appendTrace(root, event); err != nil {
		fatal(err)
	}
	var result toolResult
	switch tool {
	case "gh":
		result = runGH(root, os.Args[1:], input)
	case "codex":
		result = runCodex(root, os.Args[1:], input)
	case "claude":
		result = runClaude(root, os.Args[1:], input)
	case "awdev-check":
		result = runCheck()
	case "git":
		result = runGit(root, os.Args[1:], input)
	default:
		fatal(fmt.Errorf("unknown fake tool %q", tool))
	}
	if err := appendTrace(root, traceEvent{Phase: "complete", Tool: tool, Stdout: result.stdout, Stderr: result.stderr, ExitCode: &result.exit}); err != nil {
		fatal(err)
	}
	_, _ = os.Stdout.WriteString(result.stdout)
	_, _ = os.Stderr.WriteString(result.stderr)
	if result.exit != 0 {
		os.Exit(result.exit)
	}
}

func runGit(root string, args []string, input []byte) toolResult {
	contents, err := os.ReadFile(filepath.Join(root, ".fake", "real-git-path"))
	if err != nil {
		fatal(err)
	}
	command := exec.Command(strings.TrimSpace(string(contents)), args...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return toolResult{stdout: stdout.String(), stderr: stderr.String(), exit: exit.ExitCode()}
		}
		fatal(err)
	}
	return toolResult{stdout: stdout.String(), stderr: stderr.String()}
}

func runGH(root string, args []string, input []byte) toolResult {
	joined := strings.Join(args, " ")
	switch {
	case joined == "repo view --json nameWithOwner,defaultBranchRef":
		return toolResult{stdout: `{"nameWithOwner":"owner/repo","defaultBranchRef":{"name":"main"}}`}
	case joined == "api graphql -f query=query { viewer { login } }":
		return toolResult{stdout: `{"data":{"viewer":{"login":"operator"}}}`}
	case strings.HasPrefix(joined, "issue view 123 --repo owner/repo --json "):
		return toolResult{stdout: `{"body":"Implement safely; literal {{.WorkflowID}} and --danger remain data.","number":123,"state":"OPEN","title":"Add argv-safe journey","updatedAt":"2026-09-01T12:00:00Z","url":"https://github.com/owner/repo/issues/123"}`}
	case joined == "api --paginate --slurp /repos/owner/repo/issues/123/comments?per_page=100":
		comments := readComments(root)
		encoded, _ := json.Marshal([][]comment{comments})
		return toolResult{stdout: string(encoded)}
	case joined == "issue comment 123 --repo owner/repo --body-file -":
		comments := readComments(root)
		comments = append(comments, comment{ID: 100, HTMLURL: "https://github.com/owner/repo/issues/123#issuecomment-100", Body: string(input), CreatedAt: "2026-09-01T12:01:00Z", User: user{Login: "operator", Type: "User"}})
		writeJSON(filepath.Join(root, ".fake", "comments.json"), comments)
		return toolResult{}
	case strings.HasPrefix(joined, "pr list --repo owner/repo --head "):
		path := filepath.Join(root, ".fake", "pull-request.json")
		contents, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return toolResult{stdout: "[]"}
		}
		if err != nil {
			fatal(err)
		}
		return toolResult{stdout: fmt.Sprintf("[%s]", contents)}
	case strings.HasPrefix(joined, "pr create --repo owner/repo --head "):
		head := valueAfter(args, "--head")
		pr := map[string]any{"number": 77, "url": "https://github.com/owner/repo/pull/77", "headRefName": head, "headRepositoryOwner": map[string]string{"login": "owner"}, "baseRefName": "main", "state": "OPEN"}
		writeJSON(filepath.Join(root, ".fake", "pull-request.json"), pr)
		if readScenario(root).Mode == scenarioPRCreateFail {
			return toolResult{stderr: "simulated connection loss after PR creation\n", exit: 1}
		}
		return toolResult{stdout: "https://github.com/owner/repo/pull/77\n"}
	default:
		fatal(fmt.Errorf("unexpected gh argv: %q", args))
	}
	return toolResult{exit: 2}
}

func runCodex(root string, args []string, input []byte) toolResult {
	if len(args) < 2 || args[0] != "exec" || args[len(args)-1] != "-" {
		fatal(fmt.Errorf("unexpected codex argv: %q", args))
	}
	output := valueAfter(args, "--output-last-message")
	access := valueAfter(args, "--sandbox")
	prompt := string(input)
	settings := readScenario(root)
	if settings.Mode == scenarioCancellation {
		child := exec.Command(os.Args[0], "descendant")
		if err := child.Start(); err != nil {
			fatal(err)
		}
		writeJSON(filepath.Join(root, ".fake", "cancellation-pids.json"), map[string]int{"parent": os.Getpid(), "child": child.Process.Pid})
		for {
			time.Sleep(time.Hour)
		}
	}
	result := `{"status":"completed","summary":"done","question":""}`

	switch {
	case strings.HasPrefix(prompt, "Create an implementation specification"):
		if settings.Mode == scenarioBlocker {
			result = `{"status":"blocked","summary":"","question":"Which compatibility behavior should be used?"}`
		} else {
			writeSpec(prompt)
		}
	case strings.HasPrefix(prompt, "Continue the recorded workflow phase"):
		writeSpec(prompt)
	case strings.HasPrefix(prompt, "Implement the supplied specification"), strings.HasPrefix(prompt, "Implement the persisted issue description directly"):
		writeFile("implementation.txt", "implemented\n")
		if settings.Mode == scenarioCheckFix || settings.Mode == scenarioCheckExhaust {
			writeFile("needs-check-fix", "repair me\n")
		}
	case strings.HasPrefix(prompt, "Repair the implementation"):
		if settings.Mode != scenarioCheckExhaust {
			_ = os.Remove("needs-check-fix")
			writeFile("implementation.txt", "implemented and check-fixed\n")
		}
	case strings.HasPrefix(prompt, "Correct the implementation"):
		writeFile("implementation.txt", "implemented and review-fixed\n")
		writeFile(".review-fixed", "yes\n")
	case strings.HasPrefix(prompt, "Review the current worktree"):
		if access != "read-only" {
			fatal(fmt.Errorf("review access = %q", access))
		}
		if settings.Mode == scenarioReviewFix {
			if _, err := os.Stat(".review-fixed"); os.IsNotExist(err) {
				result = `{"approved":false,"findings":[{"severity":"high","path":"implementation.txt","line":1,"message":"Needs correction"}]}`
			} else if err != nil {
				fatal(err)
			} else {
				result = `{"approved":true,"findings":[]}`
			}
		} else if settings.Mode == scenarioReviewExhaust {
			result = `{"approved":false,"findings":[{"severity":"high","path":"implementation.txt","line":1,"message":"Still needs direction"}]}`
		} else {
			result = `{"approved":true,"findings":[]}`
		}
	default:
		fatal(fmt.Errorf("unexpected codex prompt: %.80q", prompt))
	}

	if err := os.WriteFile(output, []byte(result), 0o600); err != nil {
		fatal(err)
	}
	return toolResult{stdout: fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n", "fake-"+strconv.FormatInt(time.Now().UnixNano(), 10))}
}

// runClaude imitates the Claude Code CLI as the claudecode adapter drives it:
// stream-json events on stdout whose terminal {"type":"result"} event carries the
// schema-shaped structured_output. The write-side effects mirror runCodex exactly
// so downstream checks and diff-scope logic behave identically.
func runClaude(root string, args []string, input []byte) toolResult {
	for _, required := range []string{"--print", "--verbose", "--json-schema", "--add-dir"} {
		if !hasArg(args, required) {
			fatal(fmt.Errorf("unexpected claude argv, missing %s: %q", required, args))
		}
	}
	if got := valueAfter(args, "--output-format"); got != "stream-json" {
		fatal(fmt.Errorf("claude --output-format = %q, want stream-json", got))
	}
	if got := valueAfter(args, "--permission-prompts"); got != "none" {
		fatal(fmt.Errorf("claude --permission-prompts = %q, want none", got))
	}
	if strings.TrimSpace(valueAfter(args, "--json-schema")) == "" {
		fatal(fmt.Errorf("claude --json-schema is empty: %q", args))
	}
	sessionID := valueAfter(args, "--session-id")
	worktree := valueAfter(args, "--add-dir")
	if args[len(args)-1] != worktree {
		fatal(fmt.Errorf("claude prompt must arrive on stdin, not argv: %q", args))
	}
	if len(input) == 0 {
		fatal(fmt.Errorf("claude prompt on stdin is empty"))
	}
	access := "workspace-write"
	switch mode := valueAfter(args, "--permission-mode"); mode {
	case "plan":
		access = "read-only"
	case "acceptEdits":
		access = "workspace-write"
	default:
		fatal(fmt.Errorf("unexpected claude --permission-mode %q", mode))
	}

	settings := readScenario(root)
	if settings.Mode == scenarioCancellation {
		// Real claude flushes the system/init line, then the stream simply stops
		// when SIGTERM arrives with no terminal result event.
		emitClaudeEvent(map[string]any{
			"type": "system", "subtype": "init", "session_id": sessionID,
			"cwd": mustGetwd(), "model": "claude-fake", "permissionMode": "plan",
			"tools": []string{"Read", "Bash", "StructuredOutput"},
		})
		child := exec.Command(os.Args[0], "descendant")
		if err := child.Start(); err != nil {
			fatal(err)
		}
		writeJSON(filepath.Join(root, ".fake", "cancellation-pids.json"), map[string]int{"parent": os.Getpid(), "child": child.Process.Pid})
		for {
			time.Sleep(time.Hour)
		}
	}

	prompt := string(input)
	result := `{"status":"completed","summary":"done","question":""}`

	switch {
	case strings.HasPrefix(prompt, "Create an implementation specification"):
		if settings.Mode == scenarioBlocker {
			result = `{"status":"blocked","summary":"","question":"Which compatibility behavior should be used?"}`
		} else {
			writeSpec(prompt)
		}
	case strings.HasPrefix(prompt, "Continue the recorded workflow phase"):
		writeSpec(prompt)
	case strings.HasPrefix(prompt, "Implement the supplied specification"), strings.HasPrefix(prompt, "Implement the persisted issue description directly"):
		writeFile("implementation.txt", "implemented\n")
		if settings.Mode == scenarioCheckFix || settings.Mode == scenarioCheckExhaust {
			writeFile("needs-check-fix", "repair me\n")
		}
	case strings.HasPrefix(prompt, "Repair the implementation"):
		if settings.Mode != scenarioCheckExhaust {
			_ = os.Remove("needs-check-fix")
			writeFile("implementation.txt", "implemented and check-fixed\n")
		}
	case strings.HasPrefix(prompt, "Correct the implementation"):
		writeFile("implementation.txt", "implemented and review-fixed\n")
		writeFile(".review-fixed", "yes\n")
	case strings.HasPrefix(prompt, "Review the current worktree"):
		if access != "read-only" {
			fatal(fmt.Errorf("review access = %q", access))
		}
		if settings.Mode == scenarioReviewFix {
			if _, err := os.Stat(".review-fixed"); os.IsNotExist(err) {
				result = `{"approved":false,"findings":[{"severity":"high","path":"implementation.txt","line":1,"message":"Needs correction"}]}`
			} else if err != nil {
				fatal(err)
			} else {
				result = `{"approved":true,"findings":[]}`
			}
		} else if settings.Mode == scenarioReviewExhaust {
			result = `{"approved":false,"findings":[{"severity":"high","path":"implementation.txt","line":1,"message":"Still needs direction"}]}`
		} else {
			result = `{"approved":true,"findings":[]}`
		}
	default:
		fatal(fmt.Errorf("unexpected claude prompt: %.80q", prompt))
	}

	permissionMode := "acceptEdits"
	if access == "read-only" {
		permissionMode = "plan"
	}
	var stream bytes.Buffer
	stream.WriteString(encodeClaudeEvent(map[string]any{
		"type": "system", "subtype": "init", "session_id": sessionID,
		"cwd": mustGetwd(), "model": "claude-fake", "permissionMode": permissionMode,
		"tools": []string{"Read", "Bash", "Edit", "Write", "StructuredOutput"},
	}))
	stream.WriteString(encodeClaudeEvent(map[string]any{
		"type": "assistant", "session_id": sessionID,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Recorded the structured result."},
		}},
	}))
	stream.WriteString(encodeClaudeEvent(map[string]any{
		"type": "assistant", "session_id": sessionID,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_fake", "name": "StructuredOutput", "input": json.RawMessage(result)},
		}},
	}))
	stream.WriteString(encodeClaudeEvent(map[string]any{
		"type": "user", "session_id": sessionID,
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"tool_use_id": "toolu_fake", "type": "tool_result", "content": "Structured output provided successfully"},
		}},
		"tool_use_result": "Structured output provided successfully",
	}))
	stream.WriteString(encodeClaudeEvent(map[string]any{
		"type": "result", "subtype": "success", "session_id": sessionID,
		"is_error": false, "api_error_status": nil, "terminal_reason": "completed",
		"stop_reason": "tool_use", "num_turns": 2, "permission_denials": []any{},
		"result": result, "structured_output": json.RawMessage(result),
	}))
	return toolResult{stdout: stream.String()}
}

func encodeClaudeEvent(event map[string]any) string {
	encoded, err := json.Marshal(event)
	if err != nil {
		fatal(err)
	}
	return string(encoded) + "\n"
}

func emitClaudeEvent(event map[string]any) {
	_, _ = os.Stdout.WriteString(encodeClaudeEvent(event))
	_ = os.Stdout.Sync()
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func runCheck() toolResult {
	if _, err := os.Stat("needs-check-fix"); err == nil {
		return toolResult{stderr: "fixture requests a repair\n", exit: 1}
	} else if !os.IsNotExist(err) {
		fatal(err)
	}
	return toolResult{}
}

func writeSpec(prompt string) {
	const prefix = "Write the specification only to this exact path relative to the current worktree:\n"
	const resumePrefix = "Read the implementation specification from "
	var path string
	if index := strings.Index(prompt, prefix); index >= 0 {
		path = strings.TrimSpace(strings.SplitN(prompt[index+len(prefix):], "\n", 2)[0])
	} else if index := strings.Index(prompt, resumePrefix); index >= 0 {
		path = strings.TrimSpace(strings.SplitN(prompt[index+len(resumePrefix):], " ", 2)[0])
	}
	if path == "" {
		fatal(fmt.Errorf("specification path missing from prompt"))
	}
	writeFile(filepath.FromSlash(path), "# Fixture specification\n")
}

func writeFile(path, contents string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		fatal(err)
	}
}

func findRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if info, statErr := os.Stat(filepath.Join(directory, ".fake")); statErr == nil && info.IsDir() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("cannot find .fake fixture root from %s", mustGetwd())
		}
		directory = parent
	}
}

func appendTrace(root string, event traceEvent) error {
	path := filepath.Join(root, ".fake", "trace.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(event)
}

func oneManifestExists(root string) bool {
	matches, err := filepath.Glob(filepath.Join(root, ".awdev", "issues", "*", "manifest.json"))
	return err == nil && len(matches) == 1
}

func readScenario(root string) scenario {
	var value scenario
	readJSON(filepath.Join(root, ".fake", "scenario.json"), &value)
	return value
}

func readComments(root string) []comment {
	var comments []comment
	path := filepath.Join(root, ".fake", "comments.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	readJSON(path, &comments)
	return comments
}

func readJSON(path string, destination any) {
	contents, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(contents, destination); err != nil {
		fatal(err)
	}
}

func writeJSON(path string, value any) {
	file, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	writer := bufio.NewWriter(file)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		fatal(err)
	}
	if err := writer.Flush(); err != nil {
		fatal(err)
	}
	if err := file.Close(); err != nil {
		fatal(err)
	}
}

func valueAfter(args []string, flag string) string {
	for index := range args {
		if args[index] == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	fatal(fmt.Errorf("missing %s in argv %q", flag, args))
	return ""
}

func mustGetwd() string {
	directory, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	return directory
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
