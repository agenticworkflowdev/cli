// Package assets exposes the repository defaults embedded in the awdev binary.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed defaults
var embedded embed.FS

// Defaults returns the embedded files relative to the .awdev directory.
func Defaults() fs.FS {
	defaults, err := fs.Sub(embedded, "defaults")
	if err != nil {
		panic(err)
	}
	return defaults
}
