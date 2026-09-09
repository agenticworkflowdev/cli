// Package assets exposes the repository defaults embedded in the awdev binary.
package assets

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

//go:embed defaults repository-skill
var embedded embed.FS

// Defaults returns the embedded files relative to the .awdev directory.
//
// The provider-specific config templates (config.*.json) are excluded from this
// tree; use DefaultConfig to obtain the template for a given provider.
func Defaults() fs.FS {
	defaults, err := fs.Sub(embedded, "defaults")
	if err != nil {
		panic(err)
	}
	return defaults
}

// DefaultConfig returns the embedded .awdev/config.json template for provider.
func DefaultConfig(provider agent.Provider) ([]byte, error) {
	name, ok := defaultConfigFiles[provider]
	if !ok {
		return nil, fmt.Errorf("no default config template for agent provider %q", provider)
	}
	contents, err := fs.ReadFile(embedded, "defaults/"+name)
	if err != nil {
		return nil, fmt.Errorf("read default config template %s: %w", name, err)
	}
	return contents, nil
}

var defaultConfigFiles = map[agent.Provider]string{
	agent.ProviderCodex:      "config.codex.json",
	agent.ProviderClaudeCode: "config.claude-code.json",
}

// RepositorySkill returns the embedded optional repository agent skill relative
// to its repository skill directory.
func RepositorySkill() fs.FS {
	skill, err := fs.Sub(embedded, "repository-skill")
	if err != nil {
		panic(err)
	}
	return skill
}

// SkillMetadataFiles maps each agent provider to the skill metadata filename it
// installs under .agents/skills/awdev/agents/.
var SkillMetadataFiles = map[agent.Provider]string{
	agent.ProviderCodex:      "openai.yaml",
	agent.ProviderClaudeCode: "anthropic.yaml",
}
