package cli

import (
	"bytes"
	"testing"
)

func TestProgressReporterAnimatesOnlyOnInteractiveTerminals(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		want        string
	}{
		{
			name:        "interactive terminal",
			interactive: true,
			want:        "Creating specification. This can take a few moments...\n\x1b]8;;file:///repo/spec.md\x1b\\spec.md\x1b]8;;\x1b\\\n\r\x1b[2K⠋\r\x1b[2K",
		},
		{
			name:        "redirected output",
			interactive: false,
			want:        "Creating specification. This can take a few moments...\nspec.md\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			report := newProgressReporterForTerminal(&output, test.interactive)
			report(ProgressUpdate{Message: "Creating specification. This can take a few moments..."})
			report(ProgressUpdate{Message: "spec.md", URL: "file:///repo/spec.md"})
			report(ProgressUpdate{Message: "⠋", Transient: true})
			report(ProgressUpdate{Transient: true})

			if got := output.String(); got != test.want {
				t.Fatalf("progress output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProgressReporterSanitizesUntrustedAgentOutput(t *testing.T) {
	var output bytes.Buffer
	report := newProgressReporterForTerminal(&output, true)
	report(ProgressUpdate{Message: "safe\n\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\\rhidden", Untrusted: true})

	if got, want := output.String(), "safe\nclickhidden\n"; got != want {
		t.Fatalf("sanitized progress = %q, want %q", got, want)
	}
}

func TestProgressReporterClearsAndResumesSpinnerAroundLiveOutput(t *testing.T) {
	var output bytes.Buffer
	report := newProgressReporterForTerminal(&output, true)
	report(ProgressUpdate{Message: "⠋", Transient: true})
	report(ProgressUpdate{Message: "Agent: update", Untrusted: true})
	report(ProgressUpdate{Message: "⠙", Transient: true})
	report(ProgressUpdate{Transient: true})

	want := "\r\x1b[2K⠋\r\x1b[2KAgent: update\n\r\x1b[2K⠙\r\x1b[2K"
	if got := output.String(); got != want {
		t.Fatalf("interleaved progress = %q, want %q", got, want)
	}
}
