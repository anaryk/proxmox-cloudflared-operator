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

// confirmFlag is how the engine marks, in a problem, what waits for the
// confirmation of pco apply --confirm-deletes.
const confirmFlag = "--confirm-deletes"

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
			"With --confirm-deletes everything that waits for a confirmation is accepted at the\n" +
			"next run: the deletes the mass delete guard holds back, and what the daemon says\n" +
			"it waits for, such as guests that Proxmox no longer lists. All of it is shown first,\n" +
			"and the question needs a terminal; a script passes --yes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			s := &screen{w: cmd.OutOrStdout()}
			if confirmDeletes {
				st, err := a.state(ctx)
				if err != nil {
					return err
				}
				if !renderConfirmation(s, st) {
					// Nothing waits, so nothing is confirmed: a confirmation that
					// nobody waits for would land on deletes the admin did not see.
					confirmDeletes = false
					if st.Mode == "enforce" && !st.At.IsZero() {
						s.println("Nothing waits for a confirmation, so there is nothing to do.")
						return s.done()
					}
					s.println("Nothing waits for a confirmation, so none is given.")
				} else {
					ok, err := a.confirm(cmd, yes, "Accept this at the next run? [y/N]")
					if err != nil {
						return err
					}
					if !ok {
						return errAborted
					}
				}
			}
			if _, err := a.client().Apply(ctx, confirmDeletes, ""); err != nil {
				return a.explain(ctx, err)
			}
			if confirmDeletes {
				s.println("Deletes confirmed for the next run.")
			}
			s.println("Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status.")
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&confirmDeletes, "confirm-deletes", false, "accept what waits for a confirmation at the next run: held deletes, guests that vanished, zones and tunnels that are gone")
	addYesFlag(cmd, &yes)
	return cmd
}

// heldDeletes returns the deletes the last cycle did not carry out.
func heldDeletes(actions []reconcile.Action) []reconcile.Action {
	return slices.DeleteFunc(pending(actions), func(act reconcile.Action) bool { return !act.Destructive })
}

// waiting returns the problems of the state that say they wait for the
// confirmation. A cycle that holds plans no action, so its holds are in the
// problems only.
func waiting(problems []string) []string {
	return slices.DeleteFunc(slices.Clone(problems), func(p string) bool { return !strings.Contains(p, confirmFlag) })
}

// renderConfirmation shows what a confirmation accepts, and reports whether
// there is anything: held deletes, or problems that wait for it.
func renderConfirmation(s *screen, st engine.State) bool {
	held, waits := heldDeletes(st.Actions), waiting(st.Problems)
	if len(held) == 0 && len(waits) == 0 {
		return false
	}
	if len(held) > 0 {
		s.println("Deletes pending:")
		actionTable(s, held)
	} else {
		s.println("No deletes are held right now.")
	}
	if len(waits) > 0 {
		s.println("")
		s.println("These also wait for the confirmation, and " + confirmFlag + " accepts all of them:")
		for _, p := range waits {
			s.printf("  - %s\n", p)
		}
	}
	s.println("")
	return true
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
