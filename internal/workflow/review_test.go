package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/agenticworkflowdev/cli/internal/checks"
	"github.com/agenticworkflowdev/cli/internal/gitrepo"
	"github.com/agenticworkflowdev/cli/internal/prompt"
	"github.com/agenticworkflowdev/cli/internal/review"
	"github.com/agenticworkflowdev/cli/internal/state"
	"github.com/agenticworkflowdev/cli/internal/workflow"
)

func TestReviewImmediatelyApprovesTheCheckedDiff(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	wantEvents := []string{
		"read", "worktree:inspect", "transition:review/running", "worktree:inspect", "render:review", "worktree:capture", "agent:read-only",
		"worktree:inspect", "worktree:inspect", "decode:review", "evidence",
	}
	if !reflect.DeepEqual(fixture.events, wantEvents) {
		t.Fatalf("events = %v, want %v", fixture.events, wantEvents)
	}
	if result.Manifest.Phase != state.PhaseReview || result.Manifest.Status != state.StatusRunning || result.Manifest.Review == nil || result.Manifest.Review.Attempt != 1 || result.Manifest.Review.MaxAttempts != 3 {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
	if result.Evidence == nil || !result.Evidence.Approved || len(fixture.evidence.results) != 1 {
		t.Fatalf("result = %#v, persisted = %#v", result, fixture.evidence.results)
	}
	request := fixture.runner.requests[0]
	if request.Access != agent.AccessReadOnly || request.OutputSchema != fixture.reviewSchema {
		t.Fatalf("review request = %#v", request)
	}
	data := fixture.reviewPrompt.datas[0]
	if data.BaseSHA != fixture.state.manifest.BaseSHA || data.SpecificationPath != fixture.state.manifest.SpecificationPath || !reflect.DeepEqual(data.CheckResults, fixture.passing) {
		t.Fatalf("review prompt data = %#v", data)
	}
}

func TestReviewUsesIssueRequirementsWhenSpecificationWasSkipped(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.state.manifest.SkipSpecification = true
	fixture.state.manifest.SpecificationPath = ""
	fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review skipped specification: %v", err)
	}
	data := fixture.reviewPrompt.datas[0]
	if !result.Manifest.SkipSpecification || result.Manifest.SpecificationPath != "" || !data.SkipSpecification {
		t.Fatalf("result = %#v, prompt data = %#v", result.Manifest, data)
	}
	if data.Issue.Body != fixture.state.manifest.Issue.Body {
		t.Fatalf("prompt issue body = %q, want %q", data.Issue.Body, fixture.state.manifest.Issue.Body)
	}
}

func TestReviewRetryAfterResumedCorrectionPreservesAttemptCounter(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.state.manifest.Review = &state.ReviewCounters{Attempt: 2, MaxAttempts: 3}
	fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review retry: %v", err)
	}
	if result.Manifest.Review == nil || result.Manifest.Review.Attempt != 2 || result.Manifest.Review.MaxAttempts != 3 {
		t.Fatalf("review retry counters = %#v", result.Manifest.Review)
	}
}

func TestReviewRejectsAWorktreeChangedAfterChecksBeforeEnteringReview(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.worktree.changed = [][]string{{"unchecked.go"}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "worktree changed after deterministic checks") {
		t.Fatalf("error = %v", err)
	}
	if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusFailed || len(fixture.runner.requests) != 0 || len(fixture.evidence.results) != 0 {
		t.Fatalf("unchecked diff reached review: %#v", result)
	}
	if containsEvent(fixture.events, "transition:review/running") {
		t.Fatalf("review phase was entered for unchecked code: %v", fixture.events)
	}
}

func TestReviewCannotApproveAChangeRacingThePreReviewSnapshot(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}
	fixture.worktree.changed = [][]string{nil, nil, nil, {"unchecked.go"}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "reviewed worktree differs from checked state") {
		t.Fatalf("error = %v", err)
	}
	if result.Manifest.Phase != state.PhaseReview || result.Manifest.Status != state.StatusFailed || fixture.reviewDecoder.calls != 0 || len(fixture.evidence.results) != 0 {
		t.Fatalf("unchecked raced diff was approved: %#v", result)
	}
}

func TestReviewCorrectsFindingsRunsAllChecksAndStartsAFreshReview(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	finding := review.Finding{Severity: review.SeverityHigh, Path: "internal/run.go", Line: 17, Message: "Handle the error."}
	fixture.reviewDecoder.results = []review.Result{
		{Approved: false, Findings: []review.Finding{finding}},
		{Approved: true, Findings: []review.Finding{}},
	}
	fixture.outcomeDecoder.outcomes = []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "fixed"}}
	fixture.checkRunner.results = [][]checks.Result{fixture.passing}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if result.Manifest.Review == nil || result.Manifest.Review.Attempt != 2 || result.Evidence == nil || !result.Evidence.Approved {
		t.Fatalf("result = %#v", result)
	}
	if got := fixture.correctionPrompt.datas[0].ReviewFindings; !reflect.DeepEqual(got, []review.Finding{finding}) {
		t.Fatalf("correction findings = %#v", got)
	}
	if len(fixture.checkRunner.definitions) != 1 || !reflect.DeepEqual(fixture.checkRunner.definitions[0], fixture.definitions) {
		t.Fatalf("check definitions = %#v", fixture.checkRunner.definitions)
	}
	if got := []agent.AccessLevel{fixture.runner.requests[0].Access, fixture.runner.requests[1].Access, fixture.runner.requests[2].Access}; !reflect.DeepEqual(got, []agent.AccessLevel{agent.AccessReadOnly, agent.AccessWorkspaceWrite, agent.AccessReadOnly}) {
		t.Fatalf("agent access sequence = %v", got)
	}
	if len(fixture.evidence.results) != 2 || fixture.evidence.results[0].Approved || !fixture.evidence.results[1].Approved {
		t.Fatalf("persisted evidence = %#v", fixture.evidence.results)
	}
	for _, want := range []string{"transition:implementation/running", "diff:capture", "diff:inspect", "checks", "transition:review/running"} {
		if !containsEvent(fixture.events, want) {
			t.Fatalf("events %v do not contain %q", fixture.events, want)
		}
	}
}

func TestReviewAttemptCapProducesHumanDirectionWithoutAnotherCorrection(t *testing.T) {
	fixture := newReviewFixture(t, 1)
	fixture.reviewDecoder.results = []review.Result{{Approved: false, Findings: []review.Finding{{Severity: review.SeverityMedium, Message: "Choose an API."}}}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if result.Blocker == nil || result.Blocker.Phase != state.PhaseReview || !strings.Contains(result.Blocker.Question, "Choose an API") {
		t.Fatalf("blocker = %#v", result.Blocker)
	}
	if len(fixture.runner.requests) != 1 || len(fixture.correctionPrompt.datas) != 0 || result.Manifest.Review.Attempt != 1 {
		t.Fatalf("review cap exceeded: requests=%d corrections=%d manifest=%#v", len(fixture.runner.requests), len(fixture.correctionPrompt.datas), result.Manifest)
	}
}

func TestReviewResumeAppliesHumanDirectionRunsChecksAndStartsFreshReview(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.state.manifest.Phase = state.PhaseReview
	fixture.state.manifest.Review = &state.ReviewCounters{Attempt: 3, MaxAttempts: 3}
	fixture.state.manifest.BlockerSequence = 1
	fixture.state.manifest.Blocker = answeredBlocker(state.PhaseReview)
	fixture.outcomeDecoder.outcomes = []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "direction applied"}}
	fixture.checkRunner.results = [][]checks.Result{fixture.passing}
	fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}

	result, err := fixture.service.Resume(context.Background(), fixture.root, fixture.state.manifest.WorkflowID)
	if err != nil {
		t.Fatalf("resume review: %v", err)
	}
	if result.Evidence == nil || !result.Evidence.Approved || !reflect.DeepEqual(result.SessionIDs, []string{"session-1", "session-2"}) {
		t.Fatalf("result = %#v", result)
	}
	if got := []agent.AccessLevel{fixture.runner.requests[0].Access, fixture.runner.requests[1].Access}; !reflect.DeepEqual(got, []agent.AccessLevel{agent.AccessWorkspaceWrite, agent.AccessReadOnly}) {
		t.Fatalf("agent access sequence = %v", got)
	}
	if len(fixture.resumePrompt.datas) != 1 || fixture.resumePrompt.datas[0].Blocker == nil || fixture.resumePrompt.datas[0].Blocker.Answer != "Use option A" {
		t.Fatalf("resume prompt data = %#v", fixture.resumePrompt.datas)
	}
	if !containsEvent(fixture.events, "invalidate") || len(fixture.checkRunner.definitions) != 1 || fixture.state.manifest.Review.Attempt != 3 {
		t.Fatalf("events=%v checks=%v manifest=%#v", fixture.events, fixture.checkRunner.definitions, fixture.state.manifest)
	}
}

func TestReviewNeverExceedsAGreaterAttemptCap(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	rejected := review.Result{Approved: false, Findings: []review.Finding{{Severity: review.SeverityMedium, Message: "Still needs correction."}}}
	fixture.reviewDecoder.results = []review.Result{rejected, rejected, rejected}
	fixture.outcomeDecoder.outcomes = []agent.Outcome{
		{Status: agent.OutcomeCompleted, Summary: "first correction"},
		{Status: agent.OutcomeCompleted, Summary: "second correction"},
	}
	fixture.checkRunner.results = [][]checks.Result{fixture.passing, fixture.passing}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if result.Blocker == nil || result.Manifest.Review.Attempt != 3 {
		t.Fatalf("result = %#v", result)
	}
	if fixture.reviewDecoder.calls != 3 || fixture.outcomeDecoder.calls != 2 || len(fixture.checkRunner.definitions) != 2 || len(fixture.runner.requests) != 5 {
		t.Fatalf("calls: reviews=%d corrections=%d checks=%d agents=%d", fixture.reviewDecoder.calls, fixture.outcomeDecoder.calls, len(fixture.checkRunner.definitions), len(fixture.runner.requests))
	}
}

func TestReviewCorrectionBlockerStopsBeforeChecks(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.reviewDecoder.results = []review.Result{{Approved: false, Findings: []review.Finding{{Severity: review.SeverityHigh, Message: "Ambiguous behavior."}}}}
	fixture.outcomeDecoder.outcomes = []agent.Outcome{{Status: agent.OutcomeBlocked, Question: "Which behavior should win?"}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if result.Blocker == nil || result.Blocker.Phase != state.PhaseImplementation || result.Blocker.Question != "Which behavior should win?" || result.Manifest.Phase != state.PhaseImplementation {
		t.Fatalf("result = %#v", result)
	}
	if len(fixture.checkRunner.definitions) != 0 || result.CheckResults != nil || result.Evidence != nil {
		t.Fatalf("stale gates survived correction blocker: %#v", result)
	}
	if !fixture.evidence.invalidated {
		t.Fatal("persisted review evidence remained readable after correction began")
	}
}

func TestReviewCheckFailureAfterCorrectionFailsWithStaleEvidenceInvalidated(t *testing.T) {
	fixture := newReviewFixture(t, 3)
	fixture.reviewDecoder.results = []review.Result{{Approved: false, Findings: []review.Finding{{Severity: review.SeverityLow, Message: "Fix it."}}}}
	fixture.outcomeDecoder.outcomes = []agent.Outcome{{Status: agent.OutcomeCompleted, Summary: "fixed"}}
	failed := checks.Result{Name: "tests", Command: []string{"go", "test", "./..."}, ExitCode: 1, Stderr: "failed"}
	fixture.checkRunner.results = [][]checks.Result{{failed}}

	result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
	if err == nil || !strings.Contains(err.Error(), "failed after review correction") {
		t.Fatalf("error = %v", err)
	}
	if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusFailed || result.Evidence != nil || !reflect.DeepEqual(result.CheckResults, []checks.Result{failed}) {
		t.Fatalf("result = %#v", result)
	}
	if len(fixture.runner.requests) != 2 || len(fixture.evidence.results) != 1 {
		t.Fatalf("a stale approval path ran: requests=%d evidence=%d", len(fixture.runner.requests), len(fixture.evidence.results))
	}
	if !fixture.evidence.invalidated {
		t.Fatal("persisted review evidence was not invalidated")
	}
}

func TestReviewMutationAndEvidenceFailuresBecomeTechnicalFailures(t *testing.T) {
	t.Run("read-only mutation", func(t *testing.T) {
		fixture := newReviewFixture(t, 3)
		fixture.worktree.changed = [][]string{nil, nil, {"new.txt", "tracked.go"}}
		fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}
		result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
		if err == nil || !strings.Contains(err.Error(), "read-only review changed worktree paths") {
			t.Fatalf("error = %v", err)
		}
		if result.Manifest.Phase != state.PhaseReview || result.Manifest.Status != state.StatusFailed || fixture.reviewDecoder.calls != 0 || len(fixture.evidence.results) != 0 {
			t.Fatalf("result = %#v, decode calls = %d", result, fixture.reviewDecoder.calls)
		}
	})

	t.Run("atomic evidence persistence", func(t *testing.T) {
		fixture := newReviewFixture(t, 3)
		fixture.reviewDecoder.results = []review.Result{{Approved: true, Findings: []review.Finding{}}}
		fixture.evidence.err = errors.New("disk full")
		result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
		if err == nil || !strings.Contains(err.Error(), "persist review evidence") {
			t.Fatalf("error = %v", err)
		}
		if result.Manifest.Status != state.StatusFailed || result.Evidence != nil {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("malformed review result", func(t *testing.T) {
		fixture := newReviewFixture(t, 3)
		fixture.reviewDecoder.errors = []error{errors.New("malformed findings")}
		result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
		if err == nil || !strings.Contains(err.Error(), "validate review agent result") {
			t.Fatalf("error = %v", err)
		}
		if result.Manifest.Status != state.StatusFailed || len(fixture.evidence.results) != 0 {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("correction changed protected path", func(t *testing.T) {
		fixture := newReviewFixture(t, 3)
		fixture.reviewDecoder.results = []review.Result{{Approved: false, Findings: []review.Finding{{Severity: review.SeverityHigh, Message: "Fix it."}}}}
		fixture.diff.offending = [][]string{{"generated/output.go"}}
		result, err := fixture.service.Review(context.Background(), fixture.root, fixture.state.manifest.WorkflowID, fixture.passing, gitrepo.WorktreeBaseline{})
		if err == nil || !strings.Contains(err.Error(), "changed protected paths") {
			t.Fatalf("error = %v", err)
		}
		if result.Manifest.Phase != state.PhaseImplementation || result.Manifest.Status != state.StatusFailed || fixture.outcomeDecoder.calls != 0 || len(fixture.checkRunner.definitions) != 0 {
			t.Fatalf("result = %#v", result)
		}
	})
}

type reviewFixture struct {
	root             string
	events           []string
	state            *reviewState
	reviewPrompt     *reviewPromptFake
	correctionPrompt *reviewPromptFake
	resumePrompt     *reviewPromptFake
	runner           *reviewAgentFake
	reviewDecoder    *reviewDecoderFake
	outcomeDecoder   *reviewOutcomeDecoderFake
	checkRunner      *reviewChecksFake
	diff             *reviewDiffFake
	worktree         *worktreeStateFake
	evidence         *reviewEvidenceFake
	passing          []checks.Result
	definitions      []checks.Definition
	reviewSchema     string
	service          *workflow.ReviewService
}

func newReviewFixture(t *testing.T, maxAttempts int) *reviewFixture {
	t.Helper()
	manifest, root := implementationManifest(t)
	manifest.Phase = state.PhaseImplementation
	passing := []checks.Result{{Name: "tests", Command: []string{"go", "test", "./..."}, ExitCode: 0, Duration: time.Second}}
	definitions := []checks.Definition{{Name: "tests", Command: []string{"go", "test", "./..."}, Timeout: time.Minute}}
	fixture := &reviewFixture{root: root, passing: passing, definitions: definitions}
	fixture.state = &reviewState{manifest: manifest, events: &fixture.events}
	fixture.reviewPrompt = &reviewPromptFake{label: "review", events: &fixture.events}
	fixture.correctionPrompt = &reviewPromptFake{label: "correction", events: &fixture.events}
	fixture.resumePrompt = &reviewPromptFake{label: "resume", events: &fixture.events}
	fixture.runner = &reviewAgentFake{events: &fixture.events}
	fixture.reviewDecoder = &reviewDecoderFake{events: &fixture.events}
	fixture.outcomeDecoder = &reviewOutcomeDecoderFake{events: &fixture.events}
	fixture.checkRunner = &reviewChecksFake{events: &fixture.events}
	fixture.diff = &reviewDiffFake{events: &fixture.events}
	fixture.worktree = &worktreeStateFake{events: &fixture.events}
	fixture.evidence = &reviewEvidenceFake{events: &fixture.events}
	fixture.reviewSchema = filepath.Join(t.TempDir(), "review.schema.json")
	fixture.service = workflow.NewReviewService(
		fixture.state, fixture.state, fixture.reviewPrompt, fixture.correctionPrompt,
		fixture.runner, fixture.reviewDecoder, fixture.outcomeDecoder, fixture.checkRunner,
		fixture.diff, fixture.worktree, fixture.evidence,
		fixture.reviewSchema, filepath.Join(t.TempDir(), "agent.schema.json"), time.Minute,
		definitions, []string{"generated"}, maxAttempts, fixture.resumePrompt,
	)
	return fixture
}

type reviewState struct {
	manifest state.Manifest
	events   *[]string
}

func (fake *reviewState) Read(string, string) (state.Manifest, error) {
	*fake.events = append(*fake.events, "read")
	return fake.manifest, nil
}

func (fake *reviewState) Transition(_ string, _ string, next state.Manifest) error {
	*fake.events = append(*fake.events, "transition:"+string(next.Phase)+"/"+string(next.Status))
	fake.manifest = next
	return nil
}

type reviewPromptFake struct {
	label  string
	events *[]string
	datas  []prompt.PromptData
}

func (fake *reviewPromptFake) Render(data prompt.PromptData) (string, error) {
	*fake.events = append(*fake.events, "render:"+fake.label)
	fake.datas = append(fake.datas, data)
	return fake.label + " prompt", nil
}

type reviewAgentFake struct {
	events   *[]string
	requests []agent.Request
}

func (fake *reviewAgentFake) Run(_ context.Context, request agent.Request) (agent.RunResult, error) {
	*fake.events = append(*fake.events, "agent:"+string(request.Access))
	fake.requests = append(fake.requests, request)
	return agent.RunResult{FinalOutput: []byte("result"), SessionID: "session-" + string(rune('0'+len(fake.requests)))}, nil
}

type reviewDecoderFake struct {
	events  *[]string
	results []review.Result
	errors  []error
	calls   int
}

func (fake *reviewDecoderFake) Decode([]byte) (review.Result, error) {
	*fake.events = append(*fake.events, "decode:review")
	index := fake.calls
	fake.calls++
	var result review.Result
	if index < len(fake.results) {
		result = fake.results[index]
	}
	var err error
	if index < len(fake.errors) {
		err = fake.errors[index]
	}
	return result, err
}

type reviewOutcomeDecoderFake struct {
	events   *[]string
	outcomes []agent.Outcome
	errors   []error
	calls    int
}

func (fake *reviewOutcomeDecoderFake) Decode([]byte) (agent.Outcome, error) {
	*fake.events = append(*fake.events, "decode:correction")
	index := fake.calls
	fake.calls++
	var outcome agent.Outcome
	if index < len(fake.outcomes) {
		outcome = fake.outcomes[index]
	}
	var err error
	if index < len(fake.errors) {
		err = fake.errors[index]
	}
	return outcome, err
}

type reviewChecksFake struct {
	events      *[]string
	results     [][]checks.Result
	errors      []error
	definitions [][]checks.Definition
}

func (fake *reviewChecksFake) Run(_ context.Context, _ string, definitions []checks.Definition) ([]checks.Result, error) {
	*fake.events = append(*fake.events, "checks")
	fake.definitions = append(fake.definitions, append([]checks.Definition(nil), definitions...))
	index := len(fake.definitions) - 1
	var results []checks.Result
	if index < len(fake.results) {
		results = cloneResultsForReviewTest(fake.results[index])
	}
	var err error
	if index < len(fake.errors) {
		err = fake.errors[index]
	}
	return results, err
}

type reviewDiffFake struct {
	events    *[]string
	offending [][]string
	calls     int
}

func (fake *reviewDiffFake) Capture(gitrepo.DiffScope) (gitrepo.DiffBaseline, error) {
	*fake.events = append(*fake.events, "diff:capture")
	return gitrepo.DiffBaseline{}, nil
}

func (fake *reviewDiffFake) Inspect(context.Context, gitrepo.DiffScope, gitrepo.DiffBaseline) ([]string, error) {
	*fake.events = append(*fake.events, "diff:inspect")
	index := fake.calls
	fake.calls++
	if index < len(fake.offending) {
		return append([]string(nil), fake.offending[index]...), nil
	}
	return nil, nil
}

type worktreeStateFake struct {
	events  *[]string
	changed [][]string
	calls   int
}

func (fake *worktreeStateFake) Capture(context.Context, string) (gitrepo.WorktreeBaseline, error) {
	*fake.events = append(*fake.events, "worktree:capture")
	return gitrepo.WorktreeBaseline{}, nil
}

func (fake *worktreeStateFake) Inspect(context.Context, string, gitrepo.WorktreeBaseline) ([]string, error) {
	*fake.events = append(*fake.events, "worktree:inspect")
	index := fake.calls
	fake.calls++
	if index < len(fake.changed) {
		return append([]string(nil), fake.changed[index]...), nil
	}
	return nil, nil
}

type reviewEvidenceFake struct {
	events      *[]string
	results     []review.Result
	err         error
	invalidated bool
}

func (fake *reviewEvidenceFake) InvalidateReview(string, string) error {
	*fake.events = append(*fake.events, "invalidate")
	if fake.err != nil {
		return fake.err
	}
	fake.invalidated = true
	return nil
}

func (fake *reviewEvidenceFake) SaveReview(_ string, _ string, result review.Result) error {
	*fake.events = append(*fake.events, "evidence")
	if fake.err != nil {
		return fake.err
	}
	fake.results = append(fake.results, result)
	fake.invalidated = false
	return nil
}

func cloneResultsForReviewTest(results []checks.Result) []checks.Result {
	cloned := make([]checks.Result, len(results))
	copy(cloned, results)
	for index := range cloned {
		cloned[index].Command = append([]string(nil), results[index].Command...)
	}
	return cloned
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}
