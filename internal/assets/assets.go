// Package assets exposes the repository defaults embedded in the awdev binary.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed defaults repository-skill
var embedded embed.FS

// Defaults returns the embedded files relative to the .awdev directory.
func Defaults() fs.FS {
	defaults, err := fs.Sub(embedded, "defaults")
	if err != nil {
		panic(err)
	}
	return defaults
}

// RepositorySkill returns the embedded optional Codex skill relative to its
// repository skill directory.
func RepositorySkill() fs.FS {
	skill, err := fs.Sub(embedded, "repository-skill")
	if err != nil {
		panic(err)
	}
	return skill
}
