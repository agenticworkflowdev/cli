package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/agenticworkflowdev/cli/internal/review"
)

const reviewFilename = "review.json"

type invalidatedReview struct {
	Invalidated bool `json:"invalidated"`
}

// ReviewPath derives the controller-owned review evidence location.
func ReviewPath(controllerRoot, workflowID string) (string, error) {
	manifestPath, err := ManifestPath(controllerRoot, workflowID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(manifestPath), reviewFilename), nil
}

// SaveReview atomically replaces the latest complete review evidence.
func (store *Store) SaveReview(controllerRoot, workflowID string, result review.Result) error {
	if store == nil || store.filesystem == nil {
		return errors.New("review store is not configured")
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("validate review result: %w", err)
	}
	reviewPath, err := ReviewPath(controllerRoot, workflowID)
	if err != nil {
		return err
	}
	directory, err := store.ensureStateDirectory(controllerRoot, workflowID, false)
	if err != nil {
		return fmt.Errorf("validate workflow state directory: %w", err)
	}
	manifestPath, _ := ManifestPath(controllerRoot, workflowID)
	if err := store.validateRegularOrMissing(manifestPath, false); err != nil {
		return fmt.Errorf("validate workflow manifest path: %w", err)
	}
	if err := store.validateRegularOrMissing(reviewPath, true); err != nil {
		return fmt.Errorf("validate review evidence path: %w", err)
	}
	return store.replaceJSON(directory, reviewPath, ".review-*.tmp", "review evidence", result)
}

// InvalidateReview atomically replaces stale evidence before any correction
// can change the checked diff.
func (store *Store) InvalidateReview(controllerRoot, workflowID string) error {
	if store == nil || store.filesystem == nil {
		return errors.New("review store is not configured")
	}
	reviewPath, err := ReviewPath(controllerRoot, workflowID)
	if err != nil {
		return err
	}
	directory, err := store.ensureStateDirectory(controllerRoot, workflowID, false)
	if err != nil {
		return fmt.Errorf("validate workflow state directory: %w", err)
	}
	if err := store.validateRegularOrMissing(reviewPath, false); err != nil {
		return fmt.Errorf("validate review evidence path: %w", err)
	}
	return store.replaceJSON(directory, reviewPath, ".review-*.tmp", "review invalidation", invalidatedReview{Invalidated: true})
}

// ReadReview strictly decodes and validates the latest complete review evidence.
func (store *Store) ReadReview(controllerRoot, workflowID string) (review.Result, error) {
	if store == nil || store.filesystem == nil {
		return review.Result{}, errors.New("review store is not configured")
	}
	reviewPath, err := ReviewPath(controllerRoot, workflowID)
	if err != nil {
		return review.Result{}, err
	}
	if _, err := store.ensureStateDirectory(controllerRoot, workflowID, false); err != nil {
		return review.Result{}, fmt.Errorf("validate workflow state directory: %w", err)
	}
	if err := store.validateRegularOrMissing(reviewPath, false); err != nil {
		return review.Result{}, fmt.Errorf("validate review evidence path: %w", err)
	}
	contents, err := store.filesystem.ReadFile(reviewPath)
	if err != nil {
		return review.Result{}, fmt.Errorf("read review evidence: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err == nil && len(fields) == 1 {
		if raw, exists := fields["invalidated"]; exists {
			var invalidated bool
			if json.Unmarshal(raw, &invalidated) == nil && invalidated {
				return review.Result{}, errors.New("review evidence is invalidated")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var result review.Result
	if err := decoder.Decode(&result); err != nil {
		return review.Result{}, fmt.Errorf("decode review evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return review.Result{}, errors.New("decode review evidence: expected one JSON object")
	}
	if err := result.Validate(); err != nil {
		return review.Result{}, fmt.Errorf("validate review evidence: %w", err)
	}
	return result, nil
}
