package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const manifestFilename = "manifest.json"

// AtomicFile is the file boundary used by Store's crash-safe replacement.
type AtomicFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// SyncDirectory is a directory handle that can make a rename durable.
type SyncDirectory interface {
	Sync() error
	Close() error
}

// FileSystem is the fault-injectable filesystem seam used by Store.
type FileSystem interface {
	Mkdir(string, os.FileMode) error
	Lstat(string) (os.FileInfo, error)
	ReadDir(string) ([]os.DirEntry, error)
	CreateTemp(string, string) (AtomicFile, error)
	ReadFile(string) ([]byte, error)
	Rename(string, string) error
	Remove(string) error
	OpenDirectory(string) (SyncDirectory, error)
}

// Store atomically reads and replaces validated manifests.
type Store struct {
	filesystem FileSystem
}

// NewStore constructs a manifest store backed by the operating system.
func NewStore() *Store { return NewStoreWithFileSystem(OSFileSystem{}) }

// NewStoreWithFileSystem constructs a store using a fault-injectable filesystem.
func NewStoreWithFileSystem(filesystem FileSystem) *Store {
	return &Store{filesystem: filesystem}
}

// ManifestPath derives the only manifest location for a workflow identity.
func ManifestPath(controllerRoot, workflowID string) (string, error) {
	if err := validateWorkflowID(workflowID); err != nil {
		return "", err
	}
	if !filepath.IsAbs(controllerRoot) || filepath.Clean(controllerRoot) != controllerRoot {
		return "", errors.New("controller root must be an absolute clean path")
	}
	return filepath.Join(controllerRoot, ".awdev", "issues", workflowID, manifestFilename), nil
}

// Read returns one complete, strictly decoded, validated manifest.
func (store *Store) Read(controllerRoot, workflowID string) (Manifest, error) {
	if store == nil || store.filesystem == nil {
		return Manifest{}, errors.New("manifest store is not configured")
	}
	manifestPath, err := ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := store.ensureStateDirectory(controllerRoot, workflowID, false); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow state directory: %w", err)
	}
	if err := store.validateRegularOrMissing(manifestPath, false); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow manifest path: %w", err)
	}
	contents, err := store.filesystem.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read workflow manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode workflow manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, errors.New("decode workflow manifest: expected one JSON object")
	}
	if manifest.WorkflowID != workflowID {
		return Manifest{}, errors.New("workflow manifest identity does not match its state directory")
	}
	migrated, err := migrateManifestPaths(controllerRoot, &manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("migrate workflow manifest paths: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow manifest: %w", err)
	}
	if err := store.validateControllerPaths(controllerRoot, manifest); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow manifest: %w", err)
	}
	if migrated {
		if err := store.Save(controllerRoot, manifest); err != nil {
			return Manifest{}, fmt.Errorf("persist migrated workflow manifest: %w", err)
		}
	}
	return manifest, nil
}

// Save replaces the manifest only after a complete validated JSON document has
// been written and flushed in the destination directory.
func (store *Store) Save(controllerRoot string, manifest Manifest) error {
	if store == nil || store.filesystem == nil {
		return errors.New("manifest store is not configured")
	}
	if _, err := migrateManifestPaths(controllerRoot, &manifest); err != nil {
		return fmt.Errorf("normalize workflow manifest paths: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("validate workflow manifest: %w", err)
	}
	if err := store.validateControllerPaths(controllerRoot, manifest); err != nil {
		return fmt.Errorf("validate workflow manifest: %w", err)
	}
	manifestPath, err := ManifestPath(controllerRoot, manifest.WorkflowID)
	if err != nil {
		return err
	}
	directory, err := store.ensureStateDirectory(controllerRoot, manifest.WorkflowID, true)
	if err != nil {
		return fmt.Errorf("prepare workflow state directory: %w", err)
	}
	if err := store.validateRegularOrMissing(manifestPath, true); err != nil {
		return fmt.Errorf("validate workflow manifest path: %w", err)
	}
	return store.replaceJSON(directory, manifestPath, ".manifest-*.tmp", "workflow manifest", manifest)
}

func (store *Store) replaceJSON(directory, destination, pattern, label string, value any) error {
	return store.replaceFile(directory, destination, pattern, label, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}

func (store *Store) replaceFile(directory, destination, pattern, label string, write func(io.Writer) error) error {
	temporary, err := store.filesystem.CreateTemp(directory, pattern)
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", label, err)
	}
	temporaryPath := temporary.Name()
	if filepath.Dir(temporaryPath) != directory {
		_ = temporary.Close()
		_ = store.filesystem.Remove(temporaryPath)
		return fmt.Errorf("temporary %s was created outside its state directory", label)
	}
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = store.filesystem.Remove(temporaryPath)
	}()

	if err := write(temporary); err != nil {
		return fmt.Errorf("encode temporary %s: %w", label, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush temporary %s: %w", label, err)
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary %s: %w", label, err)
	}
	closed = true
	if err := store.filesystem.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("replace %s: %w", label, err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directoryHandle, err := store.filesystem.OpenDirectory(directory)
	if err != nil {
		return fmt.Errorf("open %s state directory for sync: %w", label, err)
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return fmt.Errorf("sync %s state directory: %w", label, err)
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close %s state directory: %w", label, err)
	}
	return nil
}

func (store *Store) ensureStateDirectory(controllerRoot, workflowID string, create bool) (string, error) {
	components := []string{
		filepath.Join(controllerRoot, ".awdev"),
		filepath.Join(controllerRoot, ".awdev", "issues"),
		filepath.Join(controllerRoot, ".awdev", "issues", workflowID),
	}
	for _, component := range components {
		info, err := store.filesystem.Lstat(component)
		if errors.Is(err, os.ErrNotExist) && create {
			mkdirErr := store.filesystem.Mkdir(component, 0o755)
			info, err = store.filesystem.Lstat(component)
			if err != nil && mkdirErr != nil {
				return "", mkdirErr
			}
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s must be a real directory, not a symlink", component)
		}
	}
	return components[len(components)-1], nil
}

func (store *Store) validateRegularOrMissing(path string, missingAllowed bool) error {
	return store.validateArtifactRegularOrMissing(path, missingAllowed, "manifest")
}

func (store *Store) validateArtifactRegularOrMissing(path string, missingAllowed bool, label string) error {
	info, err := store.filesystem.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && missingAllowed {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s path must be a regular file, not a symlink", label)
	}
	return nil
}

func (store *Store) workflowIDs(controllerRoot string) ([]string, error) {
	if !filepath.IsAbs(controllerRoot) || filepath.Clean(controllerRoot) != controllerRoot {
		return nil, errors.New("controller root must be an absolute clean path")
	}
	directory := filepath.Join(controllerRoot, ".awdev", "issues")
	info, err := store.filesystem.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("workflow state root must be a real directory, not a symlink")
	}
	entries, err := store.filesystem.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	workflowIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if validateWorkflowID(entry.Name()) == nil {
			workflowIDs = append(workflowIDs, entry.Name())
		}
	}
	return workflowIDs, nil
}

func (store *Store) validateControllerPaths(controllerRoot string, manifest Manifest) error {
	if !filepath.IsAbs(controllerRoot) || filepath.Clean(controllerRoot) != controllerRoot {
		return errors.New("controller root must be an absolute clean path")
	}
	canonicalRoot, err := filepath.EvalSymlinks(controllerRoot)
	if err != nil {
		return fmt.Errorf("resolve controller root: %w", err)
	}
	for _, directory := range []struct {
		path  string
		label string
	}{
		{path: filepath.Join(controllerRoot, ".awdev"), label: "controller .awdev directory"},
		{path: filepath.Join(controllerRoot, ".awdev", "worktrees"), label: "controller worktree directory"},
	} {
		info, statErr := store.filesystem.Lstat(directory.path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect %s: %w", directory.label, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a real directory, not a symlink", directory.label)
		}
	}
	absoluteWorktree, err := ResolveWorktreePath(controllerRoot, manifest.Worktree)
	if err != nil {
		return err
	}
	worktreeParent := filepath.Dir(absoluteWorktree)
	manifestRoot := filepath.Dir(filepath.Dir(worktreeParent))
	canonicalManifestRoot, err := filepath.EvalSymlinks(manifestRoot)
	if err != nil || canonicalManifestRoot != canonicalRoot {
		return errors.New("manifest worktree is outside the controller-owned worktree directory")
	}
	if manifest.SpecificationPath != "" {
		return verifySpecificationFile(store.filesystem, absoluteWorktree, manifest.SpecificationPath)
	}
	return nil
}

func migrateManifestPaths(controllerRoot string, manifest *Manifest) (bool, error) {
	if manifest == nil {
		return false, errors.New("manifest is required")
	}
	migrated := false
	if filepath.IsAbs(manifest.Worktree) {
		if filepath.Clean(manifest.Worktree) != manifest.Worktree {
			return false, errors.New("legacy worktree must be an absolute clean path")
		}
		relative, err := filepath.Rel(controllerRoot, manifest.Worktree)
		if err != nil {
			return false, fmt.Errorf("derive repository-relative legacy worktree: %w", err)
		}
		manifest.Worktree = filepath.ToSlash(relative)
		migrated = true
	}
	if manifest.LastError != nil {
		message := RelativizeControllerPaths(controllerRoot, manifest.LastError.Message)
		if message != manifest.LastError.Message {
			manifest.LastError.Message = message
			migrated = true
		}
	}
	return migrated, nil
}

// OSFileSystem implements FileSystem with the local operating system.
type OSFileSystem struct{}

func (OSFileSystem) Mkdir(path string, mode os.FileMode) error  { return os.Mkdir(path, mode) }
func (OSFileSystem) Lstat(path string) (os.FileInfo, error)     { return os.Lstat(path) }
func (OSFileSystem) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }
func (OSFileSystem) CreateTemp(directory, pattern string) (AtomicFile, error) {
	return os.CreateTemp(directory, pattern)
}
func (OSFileSystem) ReadFile(path string) ([]byte, error)             { return os.ReadFile(path) }
func (OSFileSystem) Rename(oldPath, newPath string) error             { return os.Rename(oldPath, newPath) }
func (OSFileSystem) Remove(path string) error                         { return os.Remove(path) }
func (OSFileSystem) OpenDirectory(path string) (SyncDirectory, error) { return os.Open(path) }
