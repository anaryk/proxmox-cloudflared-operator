package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
)

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation",
		Long: "Check what pco needs: the mode, the last cycle, the inventory, the credentials,\n" +
			"cloudflared, the tunnels and their connectors, the way out to Cloudflare, the writer, the\n" +
			"records in the way, Proxmox, the store and the lock of the node, what waits for a\n" +
			"confirmation and the guests that wait for approval. Each finding comes with what to do\n" +
			"about it, and the exit status is 1 when a check fails.\n\n" + jsonHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			raw, err := a.client().DoctorRaw(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			var findings []doctor.Finding
			decodeErr := json.Unmarshal(raw, &findings)
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			}
			if decodeErr != nil {
				return couldNotAsk{fmt.Errorf("decoding the answer of the daemon: %w", decodeErr)}
			}
			if !a.json {
				if err := renderFindings(cmd.OutOrStdout(), findings); err != nil {
					return err
				}
			}
			if doctor.Failed(findings) {
				return errReported
			}
			return nil
		},
	}
}

// renderFindings writes a finding a line, with what to do about it on the
// line below, and a summary.
func renderFindings(w io.Writer, findings []doctor.Finding) error {
	s := &screen{w: w}
	lines := make([]markedLine, len(findings))
	fails, warns := 0, 0
	for i, f := range findings {
		lines[i] = markedLine{level: f.Level, name: f.Check, detail: f.Detail, fix: f.Fix}
		switch f.Level {
		case doctor.LevelFail:
			fails++
		case doctor.LevelWarn:
			warns++
		}
	}
	renderMarked(s, lines)
	s.printf("\n%s\n", summary(fails, warns))
	return s.done()
}

func summary(fails, warns int) string {
	switch {
	case fails > 0:
		return fmt.Sprintf("%s, %s.", counted(fails, "failure"), counted(warns, "warning"))
	case warns > 0:
		return counted(warns, "warning") + ", no failure."
	}
	return "Everything is in order."
}

// counted says "no warning", "1 warning" or "2 warnings".
func counted(n int, what string) string {
	switch n {
	case 0:
		return "no " + what
	case 1:
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}
