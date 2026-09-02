//go:build unix

package state_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agenticworkflowdev/cli/internal/state"
)

func TestFileLockerReclaimsLockAfterOwnerIsKilled(t *testing.T) {
	root := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=TestFileLockOwnerHelper", "--", root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		_ = command.Process.Kill()
		t.Fatalf("lock helper readiness = %q, %v", line, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill lock owner: %v", err)
	}
	_ = command.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := state.NewFileLocker().Acquire(ctx, root, "gh-17")
	if err != nil {
		t.Fatalf("acquire after killed owner: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release reclaimed lock: %v", err)
	}
}

func TestFileLockOwnerHelper(t *testing.T) {
	separator := 0
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index + 1
			break
		}
	}
	if separator == 0 || separator >= len(os.Args) {
		return
	}
	lock, err := state.NewFileLocker().Acquire(context.Background(), os.Args[separator], "gh-17")
	if err != nil {
		os.Exit(2)
	}
	defer lock.Release()
	fmt.Println("locked")
	for {
		time.Sleep(time.Hour)
	}
}
