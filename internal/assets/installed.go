package assets

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	PromptSpec      = "spec"
	PromptImplement = "implement"
	PromptFixChecks = "fix-checks"
	PromptReview    = "review"
	PromptFixReview = "fix-review"
	PromptResume    = "resume-blocked"

	SchemaAgentResult  = "agent-result"
	SchemaReviewResult = "review-result"
)

var installedPromptFiles = map[string]string{
	PromptSpec:      "spec.md",
	PromptImplement: "implement.md",
	PromptFixChecks: "fix-checks.md",
	PromptReview:    "review.md",
	PromptFixReview: "fix-review.md",
	PromptResume:    "resume-blocked.md",
}

var installedSchemaFiles = map[string]string{
	SchemaAgentResult:  "agent-result.schema.json",
	SchemaReviewResult: "review-result.schema.json",
}

// File is one repository-owned installed asset and its absolute location.
type File struct {
	Path     string
	Contents []byte
}

// Installed is the complete set of repository-owned Slice 1 assets.
type Installed struct {
	Config  File
	Prompts map[string]File
	Schemas map[string]File
}

// LoadInstalled reads all assets relative to controllerRoot. It never falls
// back to embedded defaults, so repository customizations remain authoritative.
func LoadInstalled(controllerRoot string) (Installed, error) {
	configuration, err := readInstalledFile(filepath.Join(controllerRoot, ".awdev", "config.json"))
	if err != nil {
		return Installed{}, err
	}

	installed := Installed{
		Config:  configuration,
		Prompts: make(map[string]File, len(installedPromptFiles)),
		Schemas: make(map[string]File, len(installedSchemaFiles)),
	}
	for name, filename := range installedPromptFiles {
		file, err := readInstalledFile(filepath.Join(controllerRoot, ".awdev", "prompts", filename))
		if err != nil {
			return Installed{}, err
		}
		installed.Prompts[name] = file
	}
	for name, filename := range installedSchemaFiles {
		file, err := readInstalledFile(filepath.Join(controllerRoot, ".awdev", "schemas", filename))
		if err != nil {
			return Installed{}, err
		}
		installed.Schemas[name] = file
	}
	return installed, nil
}

func readInstalledFile(path string) (File, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read installed asset %s: %w", path, err)
	}
	return File{Path: path, Contents: contents}, nil
}
