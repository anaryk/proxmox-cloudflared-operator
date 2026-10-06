package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

func (a *app) diagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose <hostname>",
		Short: "Walk the chain of the route of a hostname",
		Long: "Walk what a request for a hostname goes through: its route, its zone, its DNS record, the\n" +
			"rule of the tunnel, the connector, the identity and the port of its target, and last a\n" +
			"request the daemon makes to that target as the tunnel would. A step that fails skips the\n" +
			"steps after it and makes the exit status 1. It changes nothing.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # Why app.example.com does not answer as it should\n" +
			"  pco diagnose app.example.com\n\n" +
			"  # The steps as JSON, for a script\n" +
			"  pco diagnose app.example.com --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			raw, err := a.client().DiagnoseRaw(ctx, args[0])
			if err != nil {
				return a.explain(ctx, err)
			}
			var steps []doctor.Step
			decodeErr := json.Unmarshal(raw, &steps)
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			}
			if decodeErr != nil {
				return couldNotAsk{fmt.Errorf("decoding the answer of the daemon: %w", decodeErr)}
			}
			if !a.json {
				if err := renderSteps(cmd.OutOrStdout(), steps); err != nil {
					return err
				}
			}
			if slices.ContainsFunc(steps, func(s doctor.Step) bool { return s.Level == doctor.LevelFail }) {
				return errReported
			}
			return nil
		},
	}
}

// renderSteps writes a step a line: its mark, its name and what it found.
func renderSteps(w io.Writer, steps []doctor.Step) error {
	s := &screen{w: w}
	lines := make([]markedLine, len(steps))
	for i, st := range steps {
		lines[i] = markedLine{level: st.Level, name: st.Name, detail: st.Detail}
	}
	renderMarked(s, lines)
	return s.done()
}

// markedLine is a line of a diagnosis or of the doctor: the level of what it
// is about, its name, what was found and, for a finding, what to do about it.
type markedLine struct {
	level             doctor.Level
	name, detail, fix string
}

// renderMarked writes a line each: its mark, its name in a column of the
// width of the longest, what was found, and a fix on the line below.
func renderMarked(s *screen, lines []markedLine) {
	width := 0
	for _, l := range lines {
		width = max(width, len([]rune(present.Printable(l.name))))
	}
	for _, l := range lines {
		s.printf("%s %-*s  %s\n", levelMark(l.level), width, l.name, l.detail)
		if l.fix != "" {
			s.printf("%s  fix: %s\n", strings.Repeat(" ", width+2), l.fix)
		}
	}
}

// levelMark is the mark of a level: ✓ ok, ! warn, ✗ fail, and ? for one this
// binary does not know.
func levelMark(l doctor.Level) string {
	switch l {
	case doctor.LevelOK:
		return "✓"
	case doctor.LevelWarn:
		return "!"
	case doctor.LevelFail:
		return "✗"
	}
	return "?"
}
