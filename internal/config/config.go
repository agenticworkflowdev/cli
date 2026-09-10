// Package config loads and validates repository-owned awdev configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/checks"
)

const (
	currentSchemaVersion  = 1
	defaultAgentTimeout   = 30 * time.Minute
	defaultCheckTimeout   = 10 * time.Minute
	defaultCheckRepairs   = 3
	defaultReviewAttempts = 3
)

// Config is validated schema-version-1 configuration.
type Config struct {
	SchemaVersion  int
	Agent          Agent
	Codex          Codex
	ClaudeCode     ClaudeCode
	Checks         []checks.Definition
	Implementation Implementation
	Review         Review
	ProtectedPaths []string
}

// Agent selects the provider and configures provider-neutral execution limits.
type Agent struct {
	Provider agent.Provider
	Timeout  time.Duration
}

// Codex configures direct Codex process invocation.
type Codex struct {
	Binary string
}

// ClaudeCode configures direct Claude Code process invocation.
type ClaudeCode struct {
	Binary string
	Model  string
}

// Implementation bounds repair-agent turns after deterministic check failures.
type Implementation struct {
	MaxCheckRepairs int
}

// Review bounds independent review-agent invocations.
type Review struct {
	MaxAttempts int
}

type rawConfig struct {
	SchemaVersion  *int               `json:"schema_version"`
	Agent          *rawAgent          `json:"agent"`
	Codex          *rawCodex          `json:"codex"`
	ClaudeCode     *rawClaudeCode     `json:"claude_code"`
	Checks         *[]rawCheck        `json:"checks"`
	Implementation *rawImplementation `json:"implementation"`
	Review         *rawReview         `json:"review"`
	ProtectedPaths []string           `json:"protected_paths"`
}

type rawAgent struct {
	Provider string  `json:"provider"`
	Timeout  *string `json:"timeout"`
}

type rawCodex struct {
	Binary string `json:"binary"`
}

type rawClaudeCode struct {
	Binary string `json:"binary"`
	Model  string `json:"model"`
}

type rawCheck struct {
	Name      string   `json:"name"`
	Directory string   `json:"directory"`
	Command   []string `json:"command"`
	Timeout   *string  `json:"timeout"`
}

type rawImplementation struct {
	MaxCheckRepairs *int `json:"max_check_repairs"`
}

type rawReview struct {
	MaxAttempts *int `json:"max_attempts"`
}

// Load reads .awdev/config.json relative to controllerRoot and validates it.
func Load(controllerRoot string) (Config, error) {
	configPath := filepath.Join(controllerRoot, ".awdev", "config.json")
	contents, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", configPath, err)
	}
	configuration, err := Parse(contents)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	return configuration, nil
}

// ValidateExisting validates an installed config when present and permits a
// missing config during first-time repository initialization.
func ValidateExisting(controllerRoot string) error {
	_, err := Load(controllerRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Parse strictly decodes and validates schema-version-1 configuration.
func Parse(contents []byte) (Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()

	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Config{}, errorsForTrailingJSON(err)
	}

	if raw.SchemaVersion == nil {
		return Config{}, fmt.Errorf("schema_version is required")
	}
	if *raw.SchemaVersion != currentSchemaVersion {
		return Config{}, fmt.Errorf("unsupported schema_version %d", *raw.SchemaVersion)
	}
	if raw.Agent == nil {
		return Config{}, fmt.Errorf("agent is required")
	}
	provider := agent.Provider(raw.Agent.Provider)
	if provider != agent.ProviderCodex && provider != agent.ProviderClaudeCode {
		return Config{}, fmt.Errorf("agent.provider must be codex or claude-code")
	}
	agentTimeout, err := positiveDuration(raw.Agent.Timeout, defaultAgentTimeout)
	if err != nil {
		return Config{}, fmt.Errorf("agent.timeout must be a positive duration")
	}

	var codexConfig Codex
	var claudeCodeConfig ClaudeCode
	switch provider {
	case agent.ProviderCodex:
		if raw.Codex == nil {
			return Config{}, fmt.Errorf("codex is required")
		}
		if strings.TrimSpace(raw.Codex.Binary) == "" {
			return Config{}, fmt.Errorf("codex.binary must not be empty")
		}
		codexConfig = Codex{Binary: raw.Codex.Binary}
		if raw.ClaudeCode != nil {
			claudeCodeConfig = ClaudeCode{Binary: raw.ClaudeCode.Binary, Model: raw.ClaudeCode.Model}
		}
	case agent.ProviderClaudeCode:
		if raw.ClaudeCode == nil {
			return Config{}, fmt.Errorf("claude_code is required")
		}
		if strings.TrimSpace(raw.ClaudeCode.Binary) == "" {
			return Config{}, fmt.Errorf("claude_code.binary must not be empty")
		}
		claudeCodeConfig = ClaudeCode{Binary: raw.ClaudeCode.Binary, Model: raw.ClaudeCode.Model}
		if raw.Codex != nil {
			codexConfig = Codex{Binary: raw.Codex.Binary}
		}
	}

	if raw.Checks == nil {
		return Config{}, fmt.Errorf("checks is required")
	}

	checkDefinitions := make([]checks.Definition, len(*raw.Checks))
	for index, check := range *raw.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return Config{}, fmt.Errorf("checks[%d].name must not be empty", index)
		}
		if len(check.Command) == 0 {
			return Config{}, fmt.Errorf("checks[%d].command must not be empty", index)
		}
		if strings.TrimSpace(check.Command[0]) == "" {
			return Config{}, fmt.Errorf("checks[%d].command[0] must not be empty", index)
		}
		if !checks.ValidDirectory(check.Directory) {
			return Config{}, fmt.Errorf("checks[%d].directory must be a repository-relative directory", index)
		}
		timeout, err := positiveDuration(check.Timeout, defaultCheckTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("checks[%d].timeout must be a positive duration", index)
		}
		checkDefinitions[index] = checks.Definition{Name: check.Name, Directory: check.Directory, Command: append([]string(nil), check.Command...), Timeout: timeout}
	}

	maxCheckRepairs := defaultCheckRepairs
	if raw.Implementation != nil && raw.Implementation.MaxCheckRepairs != nil {
		maxCheckRepairs = *raw.Implementation.MaxCheckRepairs
	}
	if maxCheckRepairs < 1 || maxCheckRepairs > 10 {
		return Config{}, fmt.Errorf("implementation.max_check_repairs must be between 1 and 10")
	}

	if raw.Review == nil {
		return Config{}, fmt.Errorf("review is required")
	}
	maxAttempts := defaultReviewAttempts
	if raw.Review.MaxAttempts != nil {
		maxAttempts = *raw.Review.MaxAttempts
	}
	if maxAttempts < 1 || maxAttempts > 10 {
		return Config{}, fmt.Errorf("review.max_attempts must be between 1 and 10")
	}

	for index, protectedPath := range raw.ProtectedPaths {
		if !validProtectedPath(protectedPath) {
			return Config{}, fmt.Errorf("protected_paths[%d] must be a repository-relative prefix", index)
		}
	}

	return Config{
		SchemaVersion:  *raw.SchemaVersion,
		Agent:          Agent{Provider: provider, Timeout: agentTimeout},
		Codex:          codexConfig,
		ClaudeCode:     claudeCodeConfig,
		Checks:         checkDefinitions,
		Implementation: Implementation{MaxCheckRepairs: maxCheckRepairs},
		Review:         Review{MaxAttempts: maxAttempts},
		ProtectedPaths: append([]string(nil), raw.ProtectedPaths...),
	}, nil
}

func positiveDuration(value *string, defaultValue time.Duration) (time.Duration, error) {
	if value == nil {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(*value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid duration")
	}
	return duration, nil
}

func validProtectedPath(value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, `\`) {
		return false
	}
	if path.IsAbs(value) || filepath.IsAbs(value) {
		return false
	}
	cleaned := path.Clean(value)
	normalized := strings.TrimSuffix(value, "/")
	return cleaned == normalized && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func errorsForTrailingJSON(err error) error {
	if err == nil {
		return fmt.Errorf("configuration must contain a single JSON object")
	}
	return fmt.Errorf("configuration must contain a single JSON object: %w", err)
}
