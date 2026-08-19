package cli_test

import (
	"bytes"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/cli"
)

func TestRootCommandPrintsHelloWorld(t *testing.T) {
	var output bytes.Buffer
	command := cli.NewRootCommand()
	command.SetOut(&output)
	command.SetArgs(nil)

	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	if got, want := output.String(), "Hello World\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
