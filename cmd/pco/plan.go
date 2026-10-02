package main

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

func (a *app) planCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Show what the daemon would change and what stands in its way",
		Long: "Show the actions of the last cycle that were not applied and why they are held,\n" +
			"the records of someone else that stand in the way of a hostname, and the names\n" +
			"that point at the tunnel but lost the marker of this install.\n\n" +
			"With --json the whole state of the daemon is printed, as it was sent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.json {
				raw, err := a.rawState(cmd.Context())
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			st, err := a.state(cmd.Context())
			if err != nil {
				return err
			}
			return renderPlan(cmd.OutOrStdout(), st)
		},
	}
}

// pending returns the actions that were not applied: the ones the daemon would
// make, or has held back.
func pending(actions []reconcile.Action) []reconcile.Action {
	var out []reconcile.Action
	for _, act := range actions {
		if !act.Applied {
			out = append(out, act)
		}
	}
	return out
}

// actionTable writes actions as a table: what they are, what they are about,
// and why they are held.
func actionTable(s *screen, actions []reconcile.Action) {
	t := s.table()
	t.row("ACTION", "TARGET", "DETAIL", "HELD")
	for _, act := range actions {
		t.row(string(act.Kind), act.Target, dash(act.Detail), dash(act.Held))
	}
	t.flush()
}

func renderPlan(w io.Writer, st engine.State) error {
	s := &screen{w: w}
	actions := pending(st.Actions)
	if len(actions) == 0 && len(st.Conflicts) == 0 && len(st.Lost) == 0 {
		s.println("Nothing to do.")
		return s.done()
	}
	first := true
	section := func(title string) {
		if !first {
			s.println("")
		}
		first = false
		if title != "" {
			s.println(title)
		}
	}

	if len(actions) > 0 {
		section("")
		actionTable(s, actions)
	}
	if len(st.Conflicts) > 0 {
		section("Records of someone else that stand in the way (pco adopt replaces one):")
		t := s.table()
		t.row("NAME", "ZONE", "TYPE", "CONTENT")
		for _, c := range st.Conflicts {
			t.row(c.Name, c.Zone, c.Type, c.Content)
		}
		t.flush()
	}
	if len(st.Lost) > 0 {
		section("Names that point at the tunnel but lost the marker of this install (pco adopt takes them back):")
		for _, name := range st.Lost {
			s.printf("  %s\n", name)
		}
	}
	return s.done()
}
