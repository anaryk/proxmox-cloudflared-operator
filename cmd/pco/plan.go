package main

import (
	"fmt"
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
			st, raw, err := a.state(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), raw)
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

func renderPlan(w io.Writer, st engine.State) error {
	actions := pending(st.Actions)
	if len(actions) == 0 && len(st.Conflicts) == 0 && len(st.Lost) == 0 {
		_, err := fmt.Fprintln(w, "Nothing to do.")
		return err
	}
	first := true
	section := func(title string) {
		if !first {
			_, _ = fmt.Fprintln(w)
		}
		first = false
		if title != "" {
			_, _ = fmt.Fprintln(w, title)
		}
	}

	if len(actions) > 0 {
		section("")
		t := newTable(w)
		_, _ = fmt.Fprintln(t, "ACTION\tTARGET\tDETAIL\tHELD")
		for _, act := range actions {
			_, _ = fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", act.Kind, act.Target, dash(act.Detail), dash(act.Held))
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}
	if len(st.Conflicts) > 0 {
		section("Records of someone else that stand in the way (pco adopt replaces one):")
		t := newTable(w)
		_, _ = fmt.Fprintln(t, "NAME\tZONE\tTYPE\tCONTENT")
		for _, c := range st.Conflicts {
			_, _ = fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", c.Name, c.Zone, c.Type, c.Content)
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}
	if len(st.Lost) > 0 {
		section("Names that point at the tunnel but lost the marker of this install (pco adopt takes them back):")
		for _, name := range st.Lost {
			_, _ = fmt.Fprintf(w, "  %s\n", name)
		}
	}
	return nil
}
