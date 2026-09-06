package codex

import (
	"sync"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/agent"
)

func TestLiveProgressWriterStopsReportingAfterClose(t *testing.T) {
	var mutex sync.Mutex
	var events []agent.ProgressEvent
	writer := newLiveProgressWriter(func(event agent.ProgressEvent) {
		mutex.Lock()
		defer mutex.Unlock()
		events = append(events, event)
	}, nil)
	writer.Observe([]byte(`{"type":"item.completed","item":{"type":"reasoning","text":"before close"}}` + "\n"))
	writer.Close()
	writer.Observe([]byte(`{"type":"item.completed","item":{"type":"reasoning","text":"after close"}}` + "\n"))

	mutex.Lock()
	defer mutex.Unlock()
	if len(events) != 1 || events[0].Message != "before close" {
		t.Fatalf("events after close = %#v", events)
	}
}
