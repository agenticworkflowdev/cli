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
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow manifest: %w", err)
	}
	if err := store.validateControllerPaths(controllerRoot, manifest); err != nil {
		return Manifest{}, fmt.Errorf("validate workflow manifest: %w", err)
	}
	return manifest, nil
}

// Save replaces the manifest only after a complete validated JSON document has
// been written and flushed in the destination directory.
func (store *Store) Save(controllerRoot string, manifest Manifest) error {
	if store == nil || store.filesystem == nil {
		return errors.New("manifest store is not configured")
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
	temporary, err := store.filesystem.CreateTemp(directory, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary workflow manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	if filepath.Dir(temporaryPath) != directory {
		_ = temporary.Close()
		_ = store.filesystem.Remove(temporaryPath)
		return errors.New("temporary workflow manifest was created outside its state directory")
	}
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = store.filesystem.Remove(temporaryPath)
	}()

	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encode temporary workflow manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush temporary workflow manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary workflow manifest: %w", err)
	}
	closed = true
	if err := store.filesystem.Rename(temporaryPath, manifestPath); err != nil {
		return fmt.Errorf("replace workflow manifest: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directoryHandle, err := store.filesystem.OpenDirectory(directory)
	if err != nil {
		return fmt.Errorf("open workflow state directory for sync: %w", err)
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return fmt.Errorf("sync workflow state directory: %w", err)
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close workflow state directory: %w", err)
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
	info, err := store.filesystem.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && missingAllowed {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("manifest path must be a regular file, not a symlink")
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
	worktreeParent := filepath.Dir(manifest.Worktree)
	manifestRoot := filepath.Dir(filepath.Dir(worktreeParent))
	canonicalManifestRoot, err := filepath.EvalSymlinks(manifestRoot)
	if err != nil || canonicalManifestRoot != canonicalRoot {
		return errors.New("manifest worktree is outside the controller-owned worktree directory")
	}
	if manifest.SpecificationPath != "" {
		for _, directory := range []string{
			filepath.Join(controllerRoot, ".awdev"),
			filepath.Join(controllerRoot, ".awdev", "specs"),
		} {
			info, statErr := store.filesystem.Lstat(directory)
			if errors.Is(statErr, os.ErrNotExist) {
				return errors.New("specification file does not exist")
			}
			if statErr != nil {
				return fmt.Errorf("inspect specification directory: %w", statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("specification directory must be a real directory, not a symlink")
			}
		}
		specificationPath := filepath.Join(controllerRoot, filepath.FromSlash(manifest.SpecificationPath))
		info, statErr := store.filesystem.Lstat(specificationPath)
		if errors.Is(statErr, os.ErrNotExist) {
			return errors.New("specification file does not exist")
		}
		if statErr != nil {
			return fmt.Errorf("inspect specification file: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("specification path must reference a regular file, not a symlink")
		}
	}
	return nil
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
