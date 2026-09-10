// Package agent defines provider-neutral agent contracts.
package agent

import "strings"

// Provider identifies an agent implementation independently of its UI label.
type Provider string

const (
	ProviderCodex      Provider = "codex"
	ProviderClaudeCode Provider = "claude-code"
)

// Valid reports whether provider identifies a supported adapter.
func (provider Provider) Valid() bool {
	return provider == ProviderCodex || provider == ProviderClaudeCode
}

// ValidSessionID reports whether value is safe to persist and pass as one
// positional provider argument.
func ValidSessionID(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	})
}

// DisplayName returns the user-facing provider name.
func (provider Provider) DisplayName() string {
	switch provider {
	case ProviderCodex:
		return "Codex"
	case ProviderClaudeCode:
		return "Claude Code"
	default:
		return string(provider)
	}
}
