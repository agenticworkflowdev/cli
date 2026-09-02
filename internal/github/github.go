// Package github provides typed GitHub operations through the gh CLI.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	processrun "github.com/agenticworkflowdev/cli/internal/process"
)

const (
	stdoutLimit = 4 << 20
	stderrLimit = 64 << 10
)

// Repository identifies the selected GitHub repository and its default branch.
type Repository struct {
	NameWithOwner string
	DefaultBranch string
}

// Actor is the authenticated GitHub user performing workflow actions.
type Actor struct {
	Login string
}

// Issue is the validated issue content used as immutable workflow input.
type Issue struct {
	Number    int
	Title     string
	Body      string
	URL       string
	State     string
	UpdatedAt time.Time
}

// Snapshot contains all GitHub data required by later bootstrap slices.
type Snapshot struct {
	Repository Repository
	Actor      Actor
	Issue      Issue
}

// Validate rejects incomplete or inconsistent data returned by GitHub.
func (snapshot Snapshot) Validate(expectedIssueNumber int) error {
	if expectedIssueNumber <= 0 {
		return errors.New("expected issue number must be positive")
	}
	if !validRepositoryIdentity(snapshot.Repository.NameWithOwner) {
		return errors.New("GitHub repository nameWithOwner is missing or malformed")
	}
	if !validBranchName(snapshot.Repository.DefaultBranch) {
		return errors.New("GitHub repository default branch is missing or malformed")
	}
	if invalidRequiredText(snapshot.Actor.Login) {
		return errors.New("authenticated GitHub actor is missing or malformed")
	}
	if snapshot.Issue.Number != expectedIssueNumber {
		return fmt.Errorf("GitHub returned issue #%d, expected #%d", snapshot.Issue.Number, expectedIssueNumber)
	}
	if invalidRequiredText(snapshot.Issue.Title) {
		return errors.New("GitHub issue title is missing or malformed")
	}
	parsedURL, err := url.Parse(snapshot.Issue.URL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return errors.New("GitHub issue URL is missing or malformed")
	}
	expectedURLPath := "/" + snapshot.Repository.NameWithOwner + "/issues/" + strconv.Itoa(expectedIssueNumber)
	if parsedURL.Path != expectedURLPath {
		return fmt.Errorf("GitHub issue URL does not match %s#%d", snapshot.Repository.NameWithOwner, expectedIssueNumber)
	}
	if snapshot.Issue.State != "OPEN" {
		if snapshot.Issue.State == "CLOSED" {
			return fmt.Errorf("GitHub issue #%d is closed", expectedIssueNumber)
		}
		return fmt.Errorf("GitHub issue state %q is invalid", snapshot.Issue.State)
	}
	if snapshot.Issue.UpdatedAt.IsZero() {
		return errors.New("GitHub issue updatedAt is missing or malformed")
	}
	return nil
}

// Fetcher obtains one GitHub snapshot. The application service revalidates the
// returned value because Fetcher implementations are injected at that boundary.
type Fetcher interface {
	Fetch(context.Context, string, int) (Snapshot, error)
}

// Client invokes gh through an injected process runner.
type Client struct {
	binary string
	runner processrun.Runner
}

// NewClient creates a GitHub client.
func NewClient(binary string, runner processrun.Runner) *Client {
	return &Client{binary: binary, runner: runner}
}

// Fetch resolves repository identity and actor before downloading the issue.
func (client *Client) Fetch(ctx context.Context, controllerRoot string, issueNumber int) (Snapshot, error) {
	if client.runner == nil {
		return Snapshot{}, errors.New("GitHub process runner is unavailable")
	}
	if strings.TrimSpace(client.binary) == "" {
		return Snapshot{}, errors.New("GitHub executable is unavailable")
	}

	repositoryOutput, err := client.run(ctx, controllerRoot, "repo", "view", "--json", "nameWithOwner,defaultBranchRef")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve GitHub repository: %w", err)
	}
	repository, err := decodeRepository(repositoryOutput)
	if err != nil {
		return Snapshot{}, err
	}

	actorOutput, err := client.run(ctx, controllerRoot, "api", "user", "--jq", "{login: .login}")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve authenticated GitHub actor: %w", err)
	}
	actor, err := decodeActor(actorOutput)
	if err != nil {
		return Snapshot{}, err
	}

	issueOutput, err := client.run(
		ctx,
		controllerRoot,
		"issue", "view", strconv.Itoa(issueNumber),
		"--repo", repository.NameWithOwner,
		"--json", "body,number,state,title,updatedAt,url",
	)
	if err != nil {
		return Snapshot{}, fmt.Errorf("fetch GitHub issue #%d: %w", issueNumber, err)
	}
	issue, err := decodeIssue(issueOutput)
	if err != nil {
		return Snapshot{}, err
	}

	snapshot := Snapshot{Repository: repository, Actor: actor, Issue: issue}
	// Validate here as well so direct Client callers receive the same contract
	// as callers going through the application service.
	if err := snapshot.Validate(issueNumber); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (client *Client) run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	request := processrun.Request{
		Directory: directory,
		Argv:      append([]string{client.binary}, arguments...),
		Environment: map[string]string{
			"GH_PROMPT_DISABLED": "1",
			"NO_COLOR":           "1",
		},
		StdoutLimit: stdoutLimit,
		StderrLimit: stderrLimit,
	}
	result, err := client.runner.Run(ctx, request)
	if result.StdoutTruncated {
		return nil, errors.New("gh stdout exceeded the capture limit")
	}
	if err != nil {
		detail := strings.TrimSpace(string(result.Stderr))
		if result.StderrTruncated {
			detail += " (truncated)"
		}
		if detail == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, detail)
	}
	return result.Stdout, nil
}

type rawRepository struct {
	NameWithOwner    *string `json:"nameWithOwner"`
	DefaultBranchRef *struct {
		Name *string `json:"name"`
	} `json:"defaultBranchRef"`
}

func decodeRepository(contents []byte) (Repository, error) {
	var raw rawRepository
	if err := decodeStrict(contents, &raw); err != nil {
		return Repository{}, fmt.Errorf("decode GitHub repository: %w", err)
	}
	if raw.NameWithOwner == nil {
		return Repository{}, errors.New("GitHub repository response is missing nameWithOwner")
	}
	if raw.DefaultBranchRef == nil || raw.DefaultBranchRef.Name == nil || strings.TrimSpace(*raw.DefaultBranchRef.Name) == "" {
		return Repository{}, errors.New("GitHub repository default branch is missing; the repository may be empty")
	}
	repository := Repository{NameWithOwner: *raw.NameWithOwner, DefaultBranch: *raw.DefaultBranchRef.Name}
	if !validRepositoryIdentity(repository.NameWithOwner) {
		return Repository{}, errors.New("GitHub repository nameWithOwner is malformed")
	}
	if !validBranchName(repository.DefaultBranch) {
		return Repository{}, errors.New("GitHub repository default branch is malformed")
	}
	return repository, nil
}

type rawActor struct {
	Login *string `json:"login"`
}

func decodeActor(contents []byte) (Actor, error) {
	var raw rawActor
	if err := decodeStrict(contents, &raw); err != nil {
		return Actor{}, fmt.Errorf("decode authenticated GitHub actor: %w", err)
	}
	if raw.Login == nil {
		return Actor{}, errors.New("authenticated GitHub actor response is missing login")
	}
	actor := Actor{Login: *raw.Login}
	if invalidRequiredText(actor.Login) {
		return Actor{}, errors.New("authenticated GitHub actor login is malformed")
	}
	return actor, nil
}

type rawIssue struct {
	Number    *int    `json:"number"`
	Title     *string `json:"title"`
	Body      *string `json:"body"`
	URL       *string `json:"url"`
	State     *string `json:"state"`
	UpdatedAt *string `json:"updatedAt"`
}

func decodeIssue(contents []byte) (Issue, error) {
	var raw rawIssue
	if err := decodeStrict(contents, &raw); err != nil {
		return Issue{}, fmt.Errorf("decode GitHub issue: %w", err)
	}
	if raw.Number == nil || raw.Title == nil || raw.Body == nil || raw.URL == nil || raw.State == nil || raw.UpdatedAt == nil {
		return Issue{}, errors.New("GitHub issue response is missing required fields")
	}
	updatedAt, err := time.Parse(time.RFC3339, *raw.UpdatedAt)
	if err != nil {
		return Issue{}, errors.New("GitHub issue updatedAt is malformed")
	}
	return Issue{
		Number:    *raw.Number,
		Title:     *raw.Title,
		Body:      *raw.Body,
		URL:       *raw.URL,
		State:     *raw.State,
		UpdatedAt: updatedAt,
	}, nil
}

func decodeStrict(contents []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("response must contain one JSON object")
		}
		return fmt.Errorf("response must contain one JSON object: %w", err)
	}
	return nil
}

func validRepositoryIdentity(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validIdentityPart(parts[0]) && validIdentityPart(parts[1])
}

func validIdentityPart(value string) bool {
	if invalidRequiredText(value) || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsFunc(value, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.')
	})
}

func validBranchName(value string) bool {
	if invalidRequiredText(value) || value == "@" || strings.HasPrefix(value, "-") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") ||
		strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func invalidRequiredText(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return true
	}
	return strings.ContainsFunc(value, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	})
}
