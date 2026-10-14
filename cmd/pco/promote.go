package main

import (
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func (a *app) promoteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Make this node the writer of a store the daemon only observes",
		Long: "Make this node the writer again after the store was restored or copied. The daemon writes\n" +
			"nothing while the store is older than what the agents remember, went back under the running\n" +
			"writer, or was made in another cluster; pco status says which. pco promote first shows what\n" +
			"differs between the store and what the agents and Cloudflare say: the epoch each agent\n" +
			"follows, the nodes, the revisions, the cluster, the generation of the sentinels. Confirmed,\n" +
			"it makes this cluster the store's and draws a new epoch above every sentinel of the install\n" +
			"at Cloudflare and every epoch an agent reported, and sets observe-only mode: pco plan shows\n" +
			"what the daemon would change, and pco apply lets it. It refuses when a sentinel cannot be\n" +
			"read, which could hold a higher generation. Host profile only; an appliance draws a new\n" +
			"epoch with pco appliance recover. Only root may, and the question needs a terminal; a script\n" +
			"passes --yes.\n\n" +
			"It asks the daemon through its socket, which answers this request for root alone; the exit\n" +
			"status is 2 when the daemon could not be asked.",
		Example: "  # See what differs, and promote this node once you agree\n" +
			"  pco promote\n\n" +
			"  # From a script that read the differences already\n" +
			"  pco promote --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			res, err := a.client().Promote(ctx, false)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			describeDifferences(s, res.Differences)
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, "Draw a new epoch and make this node the writer? [y/N]")
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			res, err = a.client().Promote(ctx, true)
			if err != nil {
				return a.explain(ctx, err)
			}
			s.printf("This node is the writer at generation %d. pco is in observe-only mode:\n"+
				"pco plan shows what it would change, and pco apply lets it.\n", res.Generation)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// describeDifferences lists what the store and the agents say differently.
func describeDifferences(s *screen, diffs []engine.DifferenceView) {
	if len(diffs) == 0 {
		s.println("The store and what the agents and Cloudflare report agree.")
		return
	}
	s.println("The store and what the agents and Cloudflare report differ:")
	for _, d := range diffs {
		s.printf("  - %s: the store says %s, reported %s\n", d.Subject, d.Store, d.Reported)
	}
}
