package diagnostic

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkingDirectoryResolutionFailureDoesNotPreventFallbackPlanning(t *testing.T) {
	absolute, reportValue := resolveWorkingDirectory("", func() (string, error) {
		return "", errors.New("directory was removed")
	})
	if absolute != "" {
		t.Fatalf("absolute directory = %q, want empty", absolute)
	}
	if !strings.Contains(reportValue, "directory was removed") {
		t.Fatalf("report directory = %q", reportValue)
	}
	for _, directory := range logDirectories(absolute) {
		if !filepath.IsAbs(directory) {
			t.Fatalf("fallback directory = %q, want absolute", directory)
		}
	}
}

func TestRelativeTemporaryDirectoryIsAnchoredToWorkingDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	got, ok := normalizeLogDirectory(filepath.Join("relative-temp", "awdev-logs"), workingDirectory)
	if !ok {
		t.Fatal("relative temporary directory was rejected despite a known working directory")
	}
	want := filepath.Join(workingDirectory, "relative-temp", "awdev-logs")
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("normalized directory = %q, want %q", got, want)
	}
}
