// Package diagnostic persists detailed local reports for command failures.
package diagnostic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

type detailer interface {
	DiagnosticDetails() string
}

// ReportError prints the concise error, saves a detailed local report, and
// prints the report's repository-relative path when it is stored in .awdev.
func ReportError(output io.Writer, workingDirectory string, arguments []string, failure error) (string, error) {
	if failure == nil {
		return "", errors.New("cannot report a nil error")
	}
	if output == nil {
		output = io.Discard
	}
	_, _ = fmt.Fprintln(output, displayProjectPaths(workingDirectory, failure.Error()))

	logPath, err := writeErrorLog(workingDirectory, arguments, failure)
	if err != nil {
		_, _ = fmt.Fprintf(output, "Error log could not be saved: %s\n", displayProjectPaths(workingDirectory, err.Error()))
		return "", err
	}
	_, _ = fmt.Fprintf(output, "Error log: %s\n", displayLogPath(workingDirectory, logPath))
	return logPath, nil
}

func displayProjectPaths(workingDirectory, message string) string {
	absoluteWorkingDirectory, _ := resolveWorkingDirectory(workingDirectory, os.Getwd)
	projectRoot := findProjectRoot(absoluteWorkingDirectory)
	if projectRoot == "" {
		return message
	}
	return state.RelativizeControllerPaths(projectRoot, message)
}

func displayLogPath(workingDirectory, logPath string) string {
	absoluteWorkingDirectory, _ := resolveWorkingDirectory(workingDirectory, os.Getwd)
	projectRoot := findProjectRoot(absoluteWorkingDirectory)
	if projectRoot == "" {
		return logPath
	}
	relative, err := filepath.Rel(projectRoot, logPath)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return logPath
	}
	relative = filepath.ToSlash(relative)
	if relative == ".awdev" || strings.HasPrefix(relative, ".awdev/") {
		return relative
	}
	return logPath
}

func writeErrorLog(workingDirectory string, arguments []string, failure error) (string, error) {
	absoluteWorkingDirectory, _ := resolveWorkingDirectory(workingDirectory, os.Getwd)
	projectRoot := findProjectRoot(absoluteWorkingDirectory)
	report := formatReport(projectRoot, absoluteWorkingDirectory, arguments, failure, time.Now().UTC())

	var failures []error
	for _, directory := range logDirectories(absoluteWorkingDirectory) {
		path, err := writePrivateFile(directory, report)
		if err == nil {
			return path, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", directory, err))
	}
	if len(failures) == 0 {
		return "", errors.New("write error log: no absolute log directory is available")
	}
	return "", fmt.Errorf("write error log: %w", errors.Join(failures...))
}

func resolveWorkingDirectory(directory string, getwd func() (string, error)) (string, string) {
	if strings.TrimSpace(directory) == "" {
		var err error
		directory, err = getwd()
		if err != nil {
			return "", fmt.Sprintf("<unavailable: %v>", err)
		}
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Sprintf("<unavailable: %v>", err)
	}
	absolute = filepath.Clean(absolute)
	return absolute, absolute
}

func logDirectories(workingDirectory string) []string {
	candidates := make([]string, 0, 3)
	if workingDirectory != "" {
		if projectRoot := findProjectRoot(workingDirectory); projectRoot != "" {
			candidates = append(candidates, filepath.Join(projectRoot, ".awdev", "logs"))
		}
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		candidates = append(candidates, filepath.Join(cache, "awdev", "logs"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "awdev-logs"))

	directories := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		absolute, ok := normalizeLogDirectory(candidate, workingDirectory)
		if ok && !seen[absolute] {
			directories = append(directories, absolute)
			seen[absolute] = true
		}
	}
	return directories
}

func normalizeLogDirectory(directory, workingDirectory string) (string, bool) {
	if !filepath.IsAbs(directory) {
		if workingDirectory == "" {
			return "", false
		}
		directory = filepath.Join(workingDirectory, directory)
	}
	return filepath.Clean(directory), true
}

func findProjectRoot(start string) string {
	for directory := start; ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(filepath.Join(directory, ".awdev"))
		if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return directory
		}
		gitInfo, gitErr := os.Lstat(filepath.Join(directory, ".git"))
		if gitErr == nil && gitInfo.Mode()&os.ModeSymlink == 0 && (gitInfo.IsDir() || gitInfo.Mode().IsRegular()) {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
	}
}

func writePrivateFile(directory string, contents []byte) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("log directory must be a real directory")
	}

	file, err := os.CreateTemp(directory, "error-*.log")
	if err != nil {
		return "", err
	}
	path := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.Write(contents); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	remove = false
	return filepath.Clean(absolutePath), nil
}

func formatReport(projectRoot, workingDirectory string, arguments []string, failure error, createdAt time.Time) []byte {
	var report bytes.Buffer
	fmt.Fprintln(&report, "awdev error report")
	fmt.Fprintf(&report, "timestamp: %s\n", createdAt.Format(time.RFC3339Nano))
	fmt.Fprintf(&report, "working_directory: %s\n", externalWorkingDirectory(projectRoot, workingDirectory))
	fmt.Fprintf(&report, "arguments: %s\n", quotedArguments(relativizeArguments(projectRoot, arguments)))
	fmt.Fprintf(&report, "error:\n%s\n", externalizePaths(projectRoot, failure.Error()))

	var diagnostic detailer
	if errors.As(failure, &diagnostic) {
		if details := strings.TrimSpace(diagnostic.DiagnosticDetails()); details != "" {
			fmt.Fprintf(&report, "diagnostics:\n%s\n", externalizePaths(projectRoot, details))
		}
	}
	return report.Bytes()
}

func externalWorkingDirectory(projectRoot, workingDirectory string) string {
	if projectRoot == "" {
		return "<outside a repository>"
	}
	relative, err := filepath.Rel(projectRoot, workingDirectory)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "<outside the repository>"
	}
	relative = filepath.ToSlash(relative)
	if relative == "." {
		return "<repository root>"
	}
	if relative == ".awdev" || strings.HasPrefix(relative, ".awdev/") {
		return relative
	}
	return "<repository path outside .awdev>"
}

func relativizeArguments(projectRoot string, arguments []string) []string {
	relative := make([]string, len(arguments))
	for index, argument := range arguments {
		relative[index] = externalizePaths(projectRoot, argument)
	}
	return relative
}

func externalizePaths(projectRoot, value string) string {
	if projectRoot == "" {
		return value
	}
	return state.RelativizeControllerPaths(projectRoot, value)
}

func quotedArguments(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = strconv.Quote(argument)
	}
	return "[" + strings.Join(quoted, " ") + "]"
}
