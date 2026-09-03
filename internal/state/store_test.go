package state_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestAtomicStoreFailuresLeaveACompleteOldOrNewManifest(t *testing.T) {
	for _, stage := range []string{"create", "write", "flush", "close", "rename", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			old := validManifest(root)
			store := state.NewStore()
			if err := store.Save(root, old); err != nil {
				t.Fatalf("save old manifest: %v", err)
			}
			next := old
			next.Phase = state.PhaseSpec

			faulty := state.NewStoreWithFileSystem(&faultFileSystem{base: state.OSFileSystem{}, stage: stage})
			if err := faulty.Save(root, next); err == nil {
				t.Fatalf("%s failure was not reported", stage)
			}
			got, err := store.Read(root, old.WorkflowID)
			if err != nil {
				t.Fatalf("reader observed invalid manifest: %v", err)
			}
			wantPhase := old.Phase
			if stage == "directory sync" {
				wantPhase = next.Phase
			}
			if got.Phase != wantPhase {
				t.Fatalf("reader observed phase %q, want complete %q manifest", got.Phase, wantPhase)
			}
		})
	}
}

func TestStoreRejectsSymlinkedStateDirectoriesBeforeWriting(t *testing.T) {
	for _, target := range []string{"issues", "workflow"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			manifest := validManifest(root)
			if err := os.Mkdir(filepath.Join(root, ".awdev"), 0o755); err != nil {
				t.Fatal(err)
			}
			switch target {
			case "issues":
				if err := os.Symlink(outside, filepath.Join(root, ".awdev", "issues")); err != nil {
					t.Fatal(err)
				}
			case "workflow":
				if err := os.Mkdir(filepath.Join(root, ".awdev", "issues"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, ".awdev", "issues", manifest.WorkflowID)); err != nil {
					t.Fatal(err)
				}
			}

			if err := state.NewStore().Save(root, manifest); err == nil {
				t.Fatal("symlinked state directory was accepted")
			}
			outsideManifest := filepath.Join(outside, "manifest.json")
			if target == "issues" {
				outsideManifest = filepath.Join(outside, manifest.WorkflowID, "manifest.json")
			}
			if _, err := os.Stat(outsideManifest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("store wrote outside controller root: %v", err)
			}
		})
	}
}

func TestStoreRejectsSymlinkedControllerWorktreeDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".awdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".awdev", "worktrees")); err != nil {
		t.Fatal(err)
	}
	manifest := validManifest(root)
	if err := state.NewStore().Save(root, manifest); err == nil || !strings.Contains(err.Error(), "worktree directory") {
		t.Fatalf("symlinked worktree directory error = %v", err)
	}
	manifestPath, err := state.ManifestPath(root, manifest.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest was persisted through symlinked worktree directory: %v", err)
	}
}

func TestStoreToleratesConcurrentStateDirectoryCreation(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	if err := os.MkdirAll(filepath.Join(root, ".awdev", "issues"), 0o755); err != nil {
		t.Fatal(err)
	}
	racedPath := filepath.Join(root, ".awdev", "issues", manifest.WorkflowID)
	filesystem := &faultFileSystem{base: state.OSFileSystem{}, racedPath: racedPath}
	if err := state.NewStoreWithFileSystem(filesystem).Save(root, manifest); err != nil {
		t.Fatalf("save after concurrent directory creation: %v", err)
	}
	if !filesystem.raceTriggered {
		t.Fatal("directory creation race was not exercised")
	}
}

func TestStoreRequiresSpecificationFileBeforeRecordingItsPath(t *testing.T) {
	root := t.TempDir()
	manifest := validManifest(root)
	manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
	store := state.NewStore()

	if err := store.Save(root, manifest); err == nil || !strings.Contains(err.Error(), "specification file does not exist") {
		t.Fatalf("save without specification error = %v", err)
	}
	// A controller-side lookalike must not satisfy the worktree postcondition.
	if err := os.MkdirAll(filepath.Join(root, ".awdev", "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifest.SpecificationPath)), []byte("# Specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(root, manifest); err == nil || !strings.Contains(err.Error(), "specification file does not exist") {
		t.Fatalf("controller-side specification satisfied worktree state: %v", err)
	}
	worktree := absoluteManifestWorktree(t, root, manifest)
	if err := os.MkdirAll(filepath.Join(worktree, ".awdev", "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, filepath.FromSlash(manifest.SpecificationPath)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(root, manifest); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty worktree specification satisfied state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, filepath.FromSlash(manifest.SpecificationPath)), []byte("# Specification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(root, manifest); err != nil {
		t.Fatalf("save with specification: %v", err)
	}
}

func TestStoreRejectsNonRegularSpecificationPaths(t *testing.T) {
	setups := map[string]func(*testing.T, string, string){
		"file symlink": func(t *testing.T, root, specificationPath string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(specificationPath), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "spec.md")
			if err := os.WriteFile(target, []byte("# Outside\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, specificationPath); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, root, specificationPath string) {
			t.Helper()
			if err := os.MkdirAll(specificationPath, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked parent": func(t *testing.T, _ string, specificationPath string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(filepath.Dir(specificationPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, filepath.Base(specificationPath)), []byte("# Outside\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Dir(specificationPath)); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := validManifest(root)
			manifest.SpecificationPath = ".awdev/specs/" + manifest.Branch + ".md"
			specificationPath := filepath.Join(absoluteManifestWorktree(t, root, manifest), filepath.FromSlash(manifest.SpecificationPath))
			setup(t, root, specificationPath)
			if err := state.NewStore().Save(root, manifest); err == nil {
				t.Fatal("non-regular specification path was accepted")
			}
			manifestPath, err := state.ManifestPath(root, manifest.WorkflowID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("manifest was persisted: %v", err)
			}
		})
	}
}

type faultFileSystem struct {
	base          state.OSFileSystem
	stage         string
	racedPath     string
	reportedGone  bool
	raceTriggered bool
}

func (filesystem *faultFileSystem) Mkdir(path string, mode os.FileMode) error {
	if path == filesystem.racedPath {
		filesystem.raceTriggered = true
		if err := filesystem.base.Mkdir(path, mode); err != nil {
			return err
		}
		return os.ErrExist
	}
	return filesystem.base.Mkdir(path, mode)
}

func (filesystem *faultFileSystem) Lstat(path string) (os.FileInfo, error) {
	if path == filesystem.racedPath && !filesystem.reportedGone {
		filesystem.reportedGone = true
		return nil, os.ErrNotExist
	}
	return filesystem.base.Lstat(path)
}

func (filesystem *faultFileSystem) ReadDir(path string) ([]os.DirEntry, error) {
	return filesystem.base.ReadDir(path)
}

func (filesystem *faultFileSystem) CreateTemp(directory, pattern string) (state.AtomicFile, error) {
	if filesystem.stage == "create" {
		return nil, errors.New("injected create failure")
	}
	file, err := filesystem.base.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	return &faultFile{AtomicFile: file, stage: filesystem.stage}, nil
}

func (filesystem *faultFileSystem) ReadFile(path string) ([]byte, error) {
	return filesystem.base.ReadFile(path)
}

func (filesystem *faultFileSystem) Rename(oldPath, newPath string) error {
	if filesystem.stage == "rename" {
		return errors.New("injected rename failure")
	}
	return filesystem.base.Rename(oldPath, newPath)
}

func (filesystem *faultFileSystem) Remove(path string) error { return filesystem.base.Remove(path) }

func (filesystem *faultFileSystem) OpenDirectory(path string) (state.SyncDirectory, error) {
	directory, err := filesystem.base.OpenDirectory(path)
	if err != nil {
		return nil, err
	}
	return &faultDirectory{SyncDirectory: directory, fail: filesystem.stage == "directory sync"}, nil
}

type faultFile struct {
	state.AtomicFile
	stage string
}

func (file *faultFile) Write(contents []byte) (int, error) {
	if file.stage == "write" {
		return 0, errors.New("injected write failure")
	}
	return file.AtomicFile.Write(contents)
}

func (file *faultFile) Sync() error {
	if file.stage == "flush" {
		return errors.New("injected flush failure")
	}
	return file.AtomicFile.Sync()
}

func (file *faultFile) Close() error {
	if file.stage == "close" {
		if closer, ok := file.AtomicFile.(io.Closer); ok {
			_ = closer.Close()
		}
		return errors.New("injected close failure")
	}
	return file.AtomicFile.Close()
}

type faultDirectory struct {
	state.SyncDirectory
	fail bool
}

func (directory *faultDirectory) Sync() error {
	if directory.fail {
		return errors.New("injected directory sync failure")
	}
	return directory.SyncDirectory.Sync()
}
