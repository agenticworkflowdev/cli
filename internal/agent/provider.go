// Package agent defines provider-neutral agent contracts.
package agent

// Provider identifies an agent implementation independently of its UI label.
type Provider string

const (
	ProviderCodex      Provider = "codex"
	ProviderClaudeCode Provider = "claude-code"
)

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
