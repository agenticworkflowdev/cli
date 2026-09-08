//go:build unix

package e2e_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInterruptTerminatesFakeAgentProcessTree(t *testing.T) {
	fixture := newFixture(t, scenarioCancellation)
	fixture.initialize(t)
	command := fixture.command("run", "github", "123")
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	pidsPath := filepath.Join(fixture.root, ".fake", "cancellation-pids.json")
	var pids map[string]int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(pidsPath); err == nil {
			var candidate map[string]int
			if json.Unmarshal(contents, &candidate) == nil && candidate["parent"] != 0 && candidate["child"] != 0 {
				pids = candidate
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pids["parent"] == 0 || pids["child"] == 0 {
		_ = command.Process.Kill()
		t.Fatalf("fake process tree did not start; output %q", output.String())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted awdev unexpectedly succeeded")
	}
	status := fixture.status(t)
	if status.Phase != "spec" || status.Status != "failed" || status.LastError == "" {
		t.Fatalf("cancelled JSON status = %#v", status)
	}

	for name, pid := range pids {
		deadline := time.Now().Add(5 * time.Second)
		for processExists(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if processExists(pid) {
			t.Errorf("fake %s process %d survived Ctrl-C", name, pid)
		}
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
