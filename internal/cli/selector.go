package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"github.com/agenticworkflowdev/cli/internal/agent"
	"github.com/spf13/cobra"
)

// selectInitOptions asks the operator which agent to configure and whether to
// install the repository skill. Both questions run in a single form so that the
// interactive selector keeps working on a real terminal (bubbletea only enables
// raw mode when its input is an *os.File) and the accessible fallback reads one
// prompt at a time.
func selectInitOptions(command *cobra.Command) (agent.Provider, bool, error) {
	var (
		provider  agent.Provider
		withSkill bool
	)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[agent.Provider]().
				Title("Select the AI:").
				Options(
					huh.NewOption("Claude Code", agent.ProviderClaudeCode),
					huh.NewOption("Codex", agent.ProviderCodex),
				).
				Value(&provider),
		),
		huh.NewGroup(
			huh.NewSelect[bool]().
				Title("Install skills:").
				Options(
					huh.NewOption("Yes", true),
					huh.NewOption("No", false),
				).
				Value(&withSkill),
		),
	).
		WithInput(initSelectorInput(command.InOrStdin())).
		WithOutput(command.OutOrStdout())

	if err := form.RunWithContext(command.Context()); err != nil {
		return "", false, fmt.Errorf("select initialization options: %w", err)
	}
	return provider, withSkill, nil
}

// initSelectorInput returns the reader the init form should consume.
//
// In accessible mode (TERM=dumb, which huh also selects for piped, non-terminal
// runs) every field builds its own bufio.Scanner; a plain reader lets the first
// field's scanner swallow the line meant for the next one, so the second prompt
// silently falls back to its default. lineReader hands out a single line per
// Read to keep the remaining input available. In interactive mode the input is
// returned untouched so bubbletea can detect a terminal and enable raw mode.
func initSelectorInput(r io.Reader) io.Reader {
	if os.Getenv("TERM") == "dumb" {
		return &lineReader{reader: bufio.NewReader(r)}
	}
	return r
}

// lineReader reads at most one line per Read call from an underlying buffered
// reader, so consecutive accessible-mode prompts sharing one stream each see
// their own line of input.
type lineReader struct {
	reader *bufio.Reader
}

func (l *lineReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, err := l.reader.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		p[n] = b
		n++
		if b == '\n' {
			break
		}
	}
	return n, nil
}
