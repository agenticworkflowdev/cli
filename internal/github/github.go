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

// AccountType is GitHub's classification for an account that authored a
// comment.
type AccountType string

const CommentAuthorTypeUser AccountType = "User"

// Repository identifies the selected GitHub repository and its default branch.
type Repository struct {
	NameWithOwner string
	DefaultBranch string
}

// Actor is the authenticated GitHub user or App performing workflow actions.
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

// CommentAuthor identifies the account that authored an issue comment.
type CommentAuthor struct {
	Login string
	Type  AccountType
}

// IssueComment is the stable, typed subset of a GitHub issue comment used by
// blocker publication and resume selection.
type IssueComment struct {
	ID        string
	URL       string
	Author    CommentAuthor
	Body      string
	CreatedAt time.Time
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

// NewClient creates a GitHub client. Authentication remains owned by gh; a
// caller can select a GitHub App installation identity with the standard
// GH_TOKEN environment variable without persisting that token in AWDev state.
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

	actor, err := client.AuthenticatedActor(ctx, controllerRoot)
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

// AuthenticatedActor resolves the user or GitHub App installation identity
// selected by gh authentication.
func (client *Client) AuthenticatedActor(ctx context.Context, controllerRoot string) (Actor, error) {
	if client.runner == nil || strings.TrimSpace(client.binary) == "" {
		return Actor{}, errors.New("GitHub client is not fully configured")
	}
	actorOutput, err := client.run(ctx, controllerRoot, "api", "graphql", "-f", "query=query { viewer { login } }")
	if err != nil {
		return Actor{}, fmt.Errorf("resolve authenticated GitHub actor: %w", err)
	}
	actor, err := decodeActor(actorOutput)
	if err != nil {
		return Actor{}, err
	}
	return actor, nil
}

// IssueUpdatedAt reads the live issue timestamp without changing workflow input.
func (client *Client) IssueUpdatedAt(ctx context.Context, controllerRoot, repository string, issueNumber int) (time.Time, error) {
	if client.runner == nil || strings.TrimSpace(client.binary) == "" {
		return time.Time{}, errors.New("GitHub client is not fully configured")
	}
	if !validRepositoryIdentity(repository) || issueNumber <= 0 {
		return time.Time{}, errors.New("GitHub issue identity is malformed")
	}
	output, err := client.run(
		ctx,
		controllerRoot,
		"issue", "view", strconv.Itoa(issueNumber),
		"--repo", repository,
		"--json", "updatedAt",
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetch GitHub issue update timestamp: %w", err)
	}
	var raw struct {
		UpdatedAt *string `json:"updatedAt"`
	}
	if err := decodeStrict(output, &raw); err != nil {
		return time.Time{}, fmt.Errorf("decode GitHub issue update timestamp: %w", err)
	}
	if raw.UpdatedAt == nil {
		return time.Time{}, errors.New("GitHub issue update timestamp is missing")
	}
	updatedAt, err := time.Parse(time.RFC3339, *raw.UpdatedAt)
	if err != nil {
		return time.Time{}, errors.New("GitHub issue update timestamp is malformed")
	}
	return updatedAt, nil
}

// ListIssueComments fetches every issue comment page in one bounded gh
// invocation. Individual malformed comments are retained as zero-value fields
// so the application service can ignore them without losing valid siblings.
func (client *Client) ListIssueComments(ctx context.Context, controllerRoot, repository string, issueNumber int) ([]IssueComment, error) {
	if client.runner == nil || strings.TrimSpace(client.binary) == "" {
		return nil, errors.New("GitHub client is not fully configured")
	}
	if !validRepositoryIdentity(repository) || issueNumber <= 0 {
		return nil, errors.New("GitHub issue identity is malformed")
	}
	endpoint := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repository, issueNumber)
	output, err := client.run(ctx, controllerRoot, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("list GitHub issue comments: %w", err)
	}
	var pages [][]rawIssueComment
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	decoder.UseNumber()
	if err := decoder.Decode(&pages); err != nil {
		return nil, fmt.Errorf("decode GitHub issue comments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("decode GitHub issue comments: expected one JSON value")
	}
	comments := make([]IssueComment, 0)
	for _, page := range pages {
		for _, raw := range page {
			comments = append(comments, raw.comment())
		}
	}
	return comments, nil
}

// PostIssueComment sends the complete untrusted body through stdin. The body
// is never included in argv, where it could be interpreted as an option.
func (client *Client) PostIssueComment(ctx context.Context, controllerRoot, repository string, issueNumber int, body string) error {
	if client.runner == nil || strings.TrimSpace(client.binary) == "" {
		return errors.New("GitHub client is not fully configured")
	}
	if !validRepositoryIdentity(repository) || issueNumber <= 0 {
		return errors.New("GitHub issue identity is malformed")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("GitHub issue comment body is empty")
	}
	_, err := client.runWithStdin(
		ctx,
		controllerRoot,
		[]byte(body),
		"issue", "comment", strconv.Itoa(issueNumber), "--repo", repository, "--body-file", "-",
	)
	if err != nil {
		return fmt.Errorf("post GitHub issue comment: %w", err)
	}
	return nil
}

func (client *Client) run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	return client.runWithStdin(ctx, directory, nil, arguments...)
}

func (client *Client) runWithStdin(ctx context.Context, directory string, stdin []byte, arguments ...string) ([]byte, error) {
	request := processrun.Request{
		Directory: directory,
		Argv:      append([]string{client.binary}, arguments...),
		Stdin:     append([]byte(nil), stdin...),
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
	Data *struct {
		Viewer *struct {
			Login *string `json:"login"`
		} `json:"viewer"`
	} `json:"data"`
}

func decodeActor(contents []byte) (Actor, error) {
	var raw rawActor
	if err := decodeStrict(contents, &raw); err != nil {
		return Actor{}, fmt.Errorf("decode authenticated GitHub actor: %w", err)
	}
	if raw.Data == nil || raw.Data.Viewer == nil || raw.Data.Viewer.Login == nil {
		return Actor{}, errors.New("authenticated GitHub actor response is missing login")
	}
	actor := Actor{Login: *raw.Data.Viewer.Login}
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

type rawIssueComment struct {
	ID     *json.Number `json:"id"`
	URL    *string      `json:"html_url"`
	Author *struct {
		Login *string `json:"login"`
		Type  *string `json:"type"`
	} `json:"user"`
	Body      *string `json:"body"`
	CreatedAt *string `json:"created_at"`
}

func (raw rawIssueComment) comment() IssueComment {
	var comment IssueComment
	if raw.ID != nil {
		comment.ID = raw.ID.String()
	}
	if raw.URL != nil {
		comment.URL = *raw.URL
	}
	if raw.Author != nil {
		if raw.Author.Login != nil {
			comment.Author.Login = *raw.Author.Login
		}
		if raw.Author.Type != nil {
			comment.Author.Type = AccountType(*raw.Author.Type)
		}
	}
	if raw.Body != nil {
		comment.Body = *raw.Body
	}
	if raw.CreatedAt != nil {
		comment.CreatedAt, _ = time.Parse(time.RFC3339, *raw.CreatedAt)
	}
	return comment
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
