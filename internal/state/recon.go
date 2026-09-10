package state

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

const reconFilename = "recon.md"

// ReconPath derives the controller-owned codebase reconnaissance artifact path.
func ReconPath(controllerRoot, workflowID string) (string, error) {
	manifestPath, err := ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(manifestPath), reconFilename), nil
}

// SaveRecon atomically replaces a complete, non-empty reconnaissance artifact.
func (store *Store) SaveRecon(controllerRoot, workflowID string, contents []byte) error {
	if store == nil || store.filesystem == nil {
		return errors.New("recon store is not configured")
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return errors.New("recon artifact must not be empty")
	}
	manifestPath, err := ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return err
	}
	directory, err := store.ensureStateDirectory(controllerRoot, workflowID, false)
	if err != nil {
		return fmt.Errorf("validate workflow state directory: %w", err)
	}
	if err := store.validateRegularOrMissing(manifestPath, false); err != nil {
		return fmt.Errorf("validate workflow manifest path: %w", err)
	}
	reconPath := filepath.Join(directory, reconFilename)
	if err := store.validateArtifactRegularOrMissing(reconPath, true, "recon"); err != nil {
		return fmt.Errorf("validate recon artifact path: %w", err)
	}
	return store.replaceFile(directory, reconPath, ".recon-*.tmp", "recon artifact", func(writer io.Writer) error {
		_, err := writer.Write(contents)
		return err
	})
}

// ReadRecon reads a complete, non-empty reconnaissance artifact.
func (store *Store) ReadRecon(controllerRoot, workflowID string) ([]byte, error) {
	if store == nil || store.filesystem == nil {
		return nil, errors.New("recon store is not configured")
	}
	reconPath, err := ReconPath(controllerRoot, workflowID)
	if err != nil {
		return nil, err
	}
	if _, err := store.ensureStateDirectory(controllerRoot, workflowID, false); err != nil {
		return nil, fmt.Errorf("validate workflow state directory: %w", err)
	}
	if err := store.validateArtifactRegularOrMissing(reconPath, false, "recon"); err != nil {
		return nil, fmt.Errorf("validate recon artifact path: %w", err)
	}
	contents, err := store.filesystem.ReadFile(reconPath)
	if err != nil {
		return nil, fmt.Errorf("read recon artifact: %w", err)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, errors.New("recon artifact must not be empty")
	}
	return contents, nil
}
