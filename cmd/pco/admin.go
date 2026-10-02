package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

const (
	applying       = "Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status."
	applyingAlways = "Observe-only mode was off already: the daemon applies changes in every cycle."
)

// sayApplied says what an apply without a confirmation did: only one that
// left observe-only mode says that the daemon starts to apply.
func sayApplied(s *screen, res engine.ApplyResult) {
	if res.LeftObserveOnly {
		s.println(applying)
		return
	}
	s.println(applyingAlways)
}

func (a *app) syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Ask the daemon for a reconcile cycle now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.client().Sync(cmd.Context()); err != nil {
				return a.explain(cmd.Context(), err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.println("A cycle was requested.")
			return s.done()
		},
	}
}

func (a *app) applyCmd() *cobra.Command {
	var confirmDeletes, yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Leave observe-only mode and start publishing",
		Long: "Leave observe-only mode: from the next cycle the daemon changes Cloudflare.\n\n" +
			"With --confirm-deletes, what the daemon shows waiting for a confirmation is listed first:\n" +
			"the DNS removals the mass delete guard holds back, guests that Proxmox no longer lists,\n" +
			"zones that left their listing and tunnels no credential sees. Confirmed, the daemon\n" +
			"accepts exactly what was listed, and refuses when that changed in the meantime: look\n" +
			"again and repeat. The question needs a terminal; a script passes --yes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if confirmDeletes {
				return a.applyConfirming(cmd, yes)
			}
			ctx := cmd.Context()
			res, err := a.client().Apply(ctx, false, "")
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			sayApplied(s, res)
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&confirmDeletes, "confirm-deletes", false, "accept what waits for a confirmation at the next run: held deletes, guests that vanished, zones and tunnels that are gone")
	addYesFlag(cmd, &yes)
	return cmd
}

// applyConfirming shows what the daemon offers to have confirmed and, when the
// admin agrees, confirms it by its offer: the daemon accepts that and nothing
// else. What it accepted is what is said to be confirmed.
func (a *app) applyConfirming(cmd *cobra.Command, yes bool) error {
	ctx := cmd.Context()
	s := &screen{w: cmd.OutOrStdout()}
	st, err := a.state(ctx)
	if err != nil {
		return err
	}
	if len(st.Waiting) > 0 {
		s.println(waitingTitle)
		renderWaiting(s, st.Waiting)
		s.println("")
	}
	if other := unaffected(st); len(other) > 0 {
		s.println(unaffectedTitle)
		actionTable(s, other)
		s.println("")
	}
	if len(st.Waiting) == 0 {
		// Nothing is confirmed: a confirmation that nothing waits for would
		// land on what the admin did not see.
		if st.Mode == "enforce" && !st.At.IsZero() {
			s.println("Nothing waits for a confirmation, so there is nothing to do.")
			return s.done()
		}
		s.println("Nothing waits for a confirmation, so none is given.")
		res, err := a.client().Apply(ctx, false, "")
		if err != nil {
			return a.explain(ctx, err)
		}
		sayApplied(s, res)
		return s.done()
	}
	if err := s.done(); err != nil {
		return err
	}
	ok, err := a.confirm(cmd, yes, "Accept this at the next run? [y/N]")
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	res, err := a.client().Apply(ctx, true, st.Offer)
	if err != nil {
		return a.explain(ctx, err)
	}
	if len(res.Accepted) == 0 {
		s.println("Nothing was confirmed.")
	} else {
		s.println("Confirmed for the next run:")
		for _, w := range res.Accepted {
			s.printf("  - %s\n", w.Detail)
		}
	}
	if res.LeftObserveOnly {
		s.println(applying)
	}
	return s.done()
}

func (a *app) adoptCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Take over the DNS record that stands in the way of a hostname",
		Long: "Replace the record of someone else that holds a hostname pco publishes, or take back\n" +
			"a record of this install that lost its marker. The conflict is shown first, and the\n" +
			"question needs a terminal; a script passes --yes. The replacement waits for a run in\n" +
			"which the tunnel is verified and its connector ready.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			name, err := hostname.Normalize(args[0])
			if err != nil {
				return fmt.Errorf("%q is not a hostname: %w", args[0], err)
			}
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			// Without a conflict in the state the daemon refuses, and says why.
			if describeConflict(s, st, name) {
				if err := s.done(); err != nil {
					return err
				}
				ok, err := a.confirm(cmd, yes, fmt.Sprintf("Adopt %s? [y/N]", name))
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
			s.printf("Adoption of %s requested; it is made by the next run that can.\n", name)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// describeConflict says what holds name according to the state, and reports
// whether anything does.
func describeConflict(s *screen, st engine.State, name string) bool {
	same := func(other string) bool { return strings.EqualFold(strings.TrimSuffix(other, "."), name) }
	if i := slices.IndexFunc(st.Conflicts, func(c reconcile.Conflict) bool { return same(c.Name) }); i >= 0 {
		c := st.Conflicts[i]
		s.printf("%s is held by a record of someone else in zone %s: %s %s.\n"+
			"Adopting replaces it with a record that points at the tunnel.\n", name, c.Zone, c.Type, c.Content)
		return true
	}
	if slices.ContainsFunc(st.Lost, same) {
		s.printf("%s points at the tunnel of this install but lost its marker.\nAdopting takes the record back.\n", name)
		return true
	}
	return false
}
