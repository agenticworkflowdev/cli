package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/config"
)

const validConfig = `{
  "schema_version": 1,
  "agent": {"provider": "codex"},
  "codex": {"binary": "codex"},
  "checks": [{"name": "tests", "directory": "services/api", "command": ["go", "test", "./..."]}],
  "review": {},
  "protected_paths": ["docs/generated", ".github/workflows/"]
}`

func TestParseAppliesDefaultsAndPreservesArgumentArrays(t *testing.T) {
	got, err := config.Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if got.SchemaVersion != 1 {
		t.Errorf("schema version = %d, want 1", got.SchemaVersion)
	}
	if got.Agent.Provider != agent.ProviderCodex {
		t.Errorf("agent provider = %q, want codex", got.Agent.Provider)
	}
	if got.Codex.Binary != "codex" {
		t.Errorf("Codex binary = %q, want codex", got.Codex.Binary)
	}
	if got.Agent.Timeout != 30*time.Minute {
		t.Errorf("agent timeout = %v, want 30m", got.Agent.Timeout)
	}
	if got.Review.MaxAttempts != 3 {
		t.Errorf("review max attempts = %d, want 3", got.Review.MaxAttempts)
	}
	if got.Implementation.MaxCheckRepairs != 3 {
		t.Errorf("implementation max check repairs = %d, want 3", got.Implementation.MaxCheckRepairs)
	}
	if got.Checks[0].Timeout != 10*time.Minute {
		t.Errorf("check timeout = %v, want 10m", got.Checks[0].Timeout)
	}
	if strings.Join(got.Checks[0].Command, "|") != "go|test|./..." {
		t.Errorf("check command = %#v, want argument array preserved", got.Checks[0].Command)
	}
	if got.Checks[0].Directory != "services/api" {
		t.Errorf("check directory = %q, want services/api", got.Checks[0].Directory)
	}
	wantPaths := "docs/generated|.github/workflows/"
	if strings.Join(got.ProtectedPaths, "|") != wantPaths {
		t.Errorf("protected paths = %#v, want %q", got.ProtectedPaths, wantPaths)
	}
}

const validClaudeCodeConfig = `{
  "schema_version": 1,
  "agent": {"provider": "claude-code"},
  "claude_code": {"binary": "claude"},
  "checks": [{"name": "tests", "directory": "services/api", "command": ["go", "test", "./..."]}],
  "review": {},
  "protected_paths": []
}`

func TestParseAcceptsClaudeCodeProvider(t *testing.T) {
	got, err := config.Parse([]byte(validClaudeCodeConfig))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if got.Agent.Provider != agent.ProviderClaudeCode {
		t.Errorf("agent provider = %q, want claude-code", got.Agent.Provider)
	}
	if got.ClaudeCode.Binary != "claude" {
		t.Errorf("ClaudeCode binary = %q, want claude", got.ClaudeCode.Binary)
	}
	if got.ClaudeCode.Model != "" {
		t.Errorf("ClaudeCode model = %q, want empty default", got.ClaudeCode.Model)
	}
	if got.Codex.Binary != "" {
		t.Errorf("Codex binary = %q, want empty when provider is claude-code", got.Codex.Binary)
	}
	if got.Agent.Timeout != 30*time.Minute {
		t.Errorf("agent timeout = %v, want 30m", got.Agent.Timeout)
	}
}

func TestParseAcceptsOptionalClaudeCodeModel(t *testing.T) {
	input := strings.Replace(validClaudeCodeConfig, `"binary": "claude"`, `"binary": "claude", "model": "claude-sonnet-4"`, 1)
	got, err := config.Parse([]byte(input))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if got.ClaudeCode.Model != "claude-sonnet-4" {
		t.Errorf("ClaudeCode model = %q, want claude-sonnet-4", got.ClaudeCode.Model)
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "unknown field", input: strings.Replace(validConfig, `"schema_version": 1,`, `"schema_version": 1, "mystery": true,`, 1), wantErr: "unknown field"},
		{name: "trailing document", input: validConfig + `{}`, wantErr: "single JSON object"},
		{name: "unsupported schema", input: strings.Replace(validConfig, `"schema_version": 1`, `"schema_version": 2`, 1), wantErr: "unsupported schema_version 2"},
		{name: "missing agent", input: strings.Replace(validConfig, "  \"agent\": {\"provider\": \"codex\"},\n", "", 1), wantErr: "agent is required"},
		{name: "unsupported provider", input: strings.Replace(validConfig, `"provider": "codex"`, `"provider": "gemini"`, 1), wantErr: "agent.provider must be codex or claude-code"},
		{name: "missing codex", input: `{"schema_version":1,"agent":{"provider":"codex"},"checks":[],"review":{}}`, wantErr: "codex is required"},
		{name: "empty binary", input: strings.Replace(validConfig, `"binary": "codex"`, `"binary": "  "`, 1), wantErr: "codex.binary must not be empty"},
		{name: "claude-code missing claude_code section", input: `{"schema_version":1,"agent":{"provider":"claude-code"},"checks":[],"review":{}}`, wantErr: "claude_code is required"},
		{name: "claude-code empty binary", input: `{"schema_version":1,"agent":{"provider":"claude-code"},"claude_code":{"binary":"  "},"checks":[],"review":{}}`, wantErr: "claude_code.binary must not be empty"},
		{name: "empty check command", input: strings.Replace(validConfig, `["go", "test", "./..."]`, `[]`, 1), wantErr: "checks[0].command must not be empty"},
		{name: "empty executable", input: strings.Replace(validConfig, `["go", "test", "./..."]`, `["", "test"]`, 1), wantErr: "checks[0].command[0] must not be empty"},
		{name: "review zero", input: strings.Replace(validConfig, `"review": {}`, `"review": {"max_attempts": 0}`, 1), wantErr: "review.max_attempts must be between 1 and 10"},
		{name: "review too high", input: strings.Replace(validConfig, `"review": {}`, `"review": {"max_attempts": 11}`, 1), wantErr: "review.max_attempts must be between 1 and 10"},
		{name: "implementation repairs zero", input: strings.Replace(validConfig, `"review": {}`, `"implementation": {"max_check_repairs": 0}, "review": {}`, 1), wantErr: "implementation.max_check_repairs must be between 1 and 10"},
		{name: "implementation repairs too high", input: strings.Replace(validConfig, `"review": {}`, `"implementation": {"max_check_repairs": 11}, "review": {}`, 1), wantErr: "implementation.max_check_repairs must be between 1 and 10"},
		{name: "zero agent timeout", input: strings.Replace(validConfig, `"provider": "codex"`, `"provider": "codex", "timeout": "0s"`, 1), wantErr: "agent.timeout must be a positive duration"},
		{name: "negative agent timeout", input: strings.Replace(validConfig, `"provider": "codex"`, `"provider": "codex", "timeout": "-1s"`, 1), wantErr: "agent.timeout must be a positive duration"},
		{name: "malformed agent timeout", input: strings.Replace(validConfig, `"provider": "codex"`, `"provider": "codex", "timeout": "soon"`, 1), wantErr: "agent.timeout must be a positive duration"},
		{name: "zero check timeout", input: strings.Replace(validConfig, `"command": ["go", "test", "./..."]`, `"command": ["go", "test", "./..."], "timeout": "0s"`, 1), wantErr: "checks[0].timeout must be a positive duration"},
		{name: "negative check timeout", input: strings.Replace(validConfig, `"command": ["go", "test", "./..."]`, `"command": ["go", "test", "./..."], "timeout": "-1s"`, 1), wantErr: "checks[0].timeout must be a positive duration"},
		{name: "malformed check timeout", input: strings.Replace(validConfig, `"command": ["go", "test", "./..."]`, `"command": ["go", "test", "./..."], "timeout": "later"`, 1), wantErr: "checks[0].timeout must be a positive duration"},
		{name: "absolute check directory", input: strings.Replace(validConfig, `"directory": "services/api"`, `"directory": "/tmp"`, 1), wantErr: "checks[0].directory must be a repository-relative directory"},
		{name: "traversing check directory", input: strings.Replace(validConfig, `"directory": "services/api"`, `"directory": "../outside"`, 1), wantErr: "checks[0].directory must be a repository-relative directory"},
		{name: "non-normal check directory", input: strings.Replace(validConfig, `"directory": "services/api"`, `"directory": "services//api"`, 1), wantErr: "checks[0].directory must be a repository-relative directory"},
		{name: "trailing slash check directory", input: strings.Replace(validConfig, `"directory": "services/api"`, `"directory": "services/api/"`, 1), wantErr: "checks[0].directory must be a repository-relative directory"},
		{name: "absolute protected path", input: strings.Replace(validConfig, `"docs/generated"`, `"/etc"`, 1), wantErr: "protected_paths[0] must be a repository-relative prefix"},
		{name: "traversing protected path", input: strings.Replace(validConfig, `"docs/generated"`, `"../outside"`, 1), wantErr: "protected_paths[0] must be a repository-relative prefix"},
		{name: "embedded traversal protected path", input: strings.Replace(validConfig, `"docs/generated"`, `"docs/../outside"`, 1), wantErr: "protected_paths[0] must be a repository-relative prefix"},
		{name: "non-normal protected path", input: strings.Replace(validConfig, `"docs/generated"`, `"docs//generated"`, 1), wantErr: "protected_paths[0] must be a repository-relative prefix"},
		{name: "empty protected path", input: strings.Replace(validConfig, `"docs/generated"`, `""`, 1), wantErr: "protected_paths[0] must be a repository-relative prefix"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Parse([]byte(test.input))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Parse() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadUsesControllerRoot(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, ".awdev")
	if err := os.Mkdir(configDirectory, 0o755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "config.json"), []byte(validConfig), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := config.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Codex.Binary != "codex" {
		t.Fatalf("Codex binary = %q, want codex", loaded.Codex.Binary)
	}
}
