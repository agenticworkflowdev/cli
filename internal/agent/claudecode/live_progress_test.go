package claudecode

import (
	"strings"
	"sync"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

func collectLiveProgress() (*liveProgressWriter, func() []agent.ProgressEvent) {
	var mutex sync.Mutex
	var events []agent.ProgressEvent
	writer := newLiveProgressWriter(func(event agent.ProgressEvent) {
		mutex.Lock()
		defer mutex.Unlock()
		events = append(events, event)
	}, []string{"secret-token"})
	return writer, func() []agent.ProgressEvent {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]agent.ProgressEvent(nil), events...)
	}
}

func TestLiveProgressWriterStopsReportingAfterClose(t *testing.T) {
	writer, snapshot := collectLiveProgress()
	writer.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"before close"}]}}` + "\n"))
	writer.Close()
	writer.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"after close"}]}}` + "\n"))

	events := snapshot()
	if len(events) != 1 || events[0].Kind != agent.ProgressMessage || events[0].Message != "before close" {
		t.Fatalf("events after close = %#v", events)
	}
}

func TestLiveProgressWriterEmitsOnlyMappedEventsAndRedacts(t *testing.T) {
	writer, snapshot := collectLiveProgress()
	writer.Observe([]byte(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"rate_limit_event","session_id":"s"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo secret-token"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"secret-token\nExit code 3"}]},"tool_use_result":{"stdout":"secret-token\n","stderr":""}}`,
		"",
	}, "\n")))
	writer.Close()

	events := snapshot()
	if len(events) != 2 {
		t.Fatalf("mapped events = %#v", events)
	}
	if events[0].Kind != agent.ProgressCommand || events[0].Message != "echo [REDACTED]" {
		t.Fatalf("command event = %#v", events[0])
	}
	if events[1].Kind != agent.ProgressCommandOutput || events[1].Message != "[REDACTED]\n" {
		t.Fatalf("command output event = %#v", events[1])
	}
	if events[1].ExitCode == nil || *events[1].ExitCode != 3 {
		t.Fatalf("command output exit code = %#v", events[1].ExitCode)
	}
}

func TestLiveProgressWriterHandlesPartialLines(t *testing.T) {
	writer, snapshot := collectLiveProgress()
	full := `{"type":"assistant","message":{"content":[{"type":"text","text":"streamed in halves"}]}}` + "\n"
	writer.Observe([]byte(full[:20]))
	writer.Observe([]byte(full[20:50]))
	writer.Observe([]byte(full[50:]))
	writer.Close()

	events := snapshot()
	if len(events) != 1 || events[0].Message != "streamed in halves" {
		t.Fatalf("events = %#v", events)
	}
}

func TestLiveProgressWriterDropsOverlongLinesWithoutBlockingLaterEvents(t *testing.T) {
	writer, snapshot := collectLiveProgress()
	giant := strings.Repeat("x", liveEventLimit+4096)
	writer.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + giant + `"}]}}` + "\n"))
	writer.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"after overflow"}]}}` + "\n"))
	writer.Close()

	events := snapshot()
	if len(events) != 1 || events[0].Message != "after overflow" {
		t.Fatalf("events after overflow = %#v", events)
	}
}
