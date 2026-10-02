package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
)

func (a *app) diagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose <hostname>",
		Short: "Walk the chain of the route of a hostname",
		Long: "Walk what a request for a hostname goes through: its route, its zone, its DNS record, the\n" +
			"rule of the tunnel, the connector, the identity and the port of its target, and last a\n" +
			"request the daemon makes to that target as the tunnel would. A step that fails skips the\n" +
			"steps after it and makes the exit status 1.\n\n" + jsonHelp,
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
				return fmt.Errorf("decoding the answer of the daemon: %w", decodeErr)
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
	width := 0
	for _, st := range steps {
		width = max(width, len([]rune(printable(st.Name))))
	}
	for _, st := range steps {
		s.printf("%s %-*s  %s\n", levelMark(st.Level), width, st.Name, st.Detail)
	}
	return s.done()
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
