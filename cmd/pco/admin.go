package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

func (a *app) syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Ask the daemon for a reconcile cycle now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.client().Sync(cmd.Context()); err != nil {
				return a.explain(cmd.Context(), err)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "A cycle was requested.")
			return err
		},
	}
}

func (a *app) applyCmd() *cobra.Command {
	var confirmDeletes, yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Leave observe-only mode and start publishing",
		Long: "Leave observe-only mode: from the next cycle the daemon changes Cloudflare.\n\n" +
			"With --confirm-deletes the deletes that the mass delete guard holds back are let\n" +
			"through at the next run. The pending ones are shown first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if confirmDeletes {
				st, _, err := a.state(ctx)
				if err != nil {
					return err
				}
				renderHeldDeletes(cmd.OutOrStdout(), st.Actions)
				ok, err := confirm(cmd, yes, "Let these deletes through at the next run? [y/N]")
				if err != nil {
					return err
				}
				if !ok {
					return errAborted
				}
			}
			if err := a.client().Apply(ctx, confirmDeletes); err != nil {
				return a.explain(ctx, err)
			}
			msg := "Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status."
			if confirmDeletes {
				msg = "Deletes confirmed for the next run. " + msg
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), msg)
			return err
		},
	}
	cmd.Flags().BoolVar(&confirmDeletes, "confirm-deletes", false, "let the deletes held by the mass delete guard through at the next run")
	addYesFlag(cmd, &yes)
	return cmd
}

// heldDeletes returns the deletes the last cycle did not carry out.
func heldDeletes(actions []reconcile.Action) []reconcile.Action {
	return slices.DeleteFunc(pending(actions), func(act reconcile.Action) bool { return !act.Destructive })
}

func renderHeldDeletes(w io.Writer, actions []reconcile.Action) {
	held := heldDeletes(actions)
	if len(held) == 0 {
		_, _ = fmt.Fprintln(w, "No deletes are held right now.")
		return
	}
	_, _ = fmt.Fprintln(w, "Deletes pending:")
	t := newTable(w)
	_, _ = fmt.Fprintln(t, "ACTION\tTARGET\tDETAIL\tHELD")
	for _, act := range held {
		_, _ = fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", act.Kind, act.Target, dash(act.Detail), dash(act.Held))
	}
	_ = t.Flush()
}

func (a *app) adoptCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Take over the DNS record that stands in the way of a hostname",
		Long: "Replace the record of someone else that holds a hostname pco publishes, or take back\n" +
			"a record of this install that lost its marker. The conflict is shown first. The\n" +
			"replacement waits for a run in which the tunnel is verified and its connector ready.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name, err := hostname.Normalize(args[0])
			if err != nil {
				return fmt.Errorf("%q is not a hostname: %w", args[0], err)
			}
			st, _, err := a.state(ctx)
			if err != nil {
				return err
			}
			// Without a conflict in the state the daemon refuses, and says why.
			if what := describeConflict(st, name); what != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), what)
				ok, err := confirm(cmd, yes, fmt.Sprintf("Adopt %s? [y/N]", name))
				if err != nil {
					return err
				}
				if !ok {
					return errAborted
				}
			}
			if err := a.client().Adopt(ctx, name); err != nil {
				return a.explain(ctx, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Adoption of %s requested; it is made by the next run that can.\n", name)
			return err
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// describeConflict says what holds name according to the state, or returns an
// empty string when nothing does.
func describeConflict(st engine.State, name string) string {
	same := func(other string) bool { return strings.EqualFold(strings.TrimSuffix(other, "."), name) }
	if i := slices.IndexFunc(st.Conflicts, func(c reconcile.Conflict) bool { return same(c.Name) }); i >= 0 {
		c := st.Conflicts[i]
		return fmt.Sprintf("%s is held by a record of someone else in zone %s: %s %s.\nAdopting replaces it with a record that points at the tunnel.",
			name, c.Zone, c.Type, c.Content)
	}
	if slices.ContainsFunc(st.Lost, same) {
		return fmt.Sprintf("%s points at the tunnel of this install but lost its marker.\nAdopting takes the record back.", name)
	}
	return ""
}
