package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/agenticworkflowdev/cli/internal/cli"
)

func TestRootCommandSavesSelectedAgent(t *testing.T) {
	t.Setenv("TERM", "dumb")

	tests := []struct {
		name      string
		input     string
		wantAgent string
	}{
		{name: "Claude Code", input: "1\n", wantAgent: "Claude Code"},
		{name: "Codex", input: "2\n", wantAgent: "Codex"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			var selectedAgent string
			command := cli.NewRootCommand(&selectedAgent)
			command.SetIn(strings.NewReader(test.input))
			command.SetOut(&output)
			command.SetArgs(nil)

			if err := command.Execute(); err != nil {
				t.Fatalf("execute command: %v", err)
			}

			if got := selectedAgent; got != test.wantAgent {
				t.Fatalf("selected agent = %q, want %q", got, test.wantAgent)
			}
			if !strings.Contains(output.String(), "Select the AI:") {
				t.Errorf("output %q does not contain prompt title", output.String())
			}
			for _, option := range []string{"Claude Code", "Codex"} {
				if !strings.Contains(output.String(), option) {
					t.Errorf("output %q does not contain option %q", output.String(), option)
				}
			}
		})
	}
}
