package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type specificationFileSystem interface {
	Lstat(string) (os.FileInfo, error)
}

// VerifySpecificationFile enforces the filesystem invariant required before a
// worktree-relative specification path can become durable workflow state.
func VerifySpecificationFile(worktree, relativePath string) error {
	return verifySpecificationFile(OSFileSystem{}, worktree, relativePath)
}

func verifySpecificationFile(filesystem specificationFileSystem, worktree, relativePath string) error {
	absolutePath := filepath.Join(worktree, filepath.FromSlash(relativePath))
	for _, directory := range []string{worktree, filepath.Join(worktree, ".awdev"), filepath.Join(worktree, ".awdev", "specs")} {
		info, err := filesystem.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("specification file does not exist")
		}
		if err != nil {
			return fmt.Errorf("inspect specification directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("specification directory must be a real directory, not a symlink")
		}
	}
	info, err := filesystem.Lstat(absolutePath)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("specification file does not exist")
	}
	if err != nil {
		return fmt.Errorf("inspect specification file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("specification path must reference a regular file, not a symlink")
	}
	if info.Size() == 0 {
		return errors.New("specification file must not be empty")
	}
	return nil
}
