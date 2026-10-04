package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func (a *app) tunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Act on the tunnels of the install",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(a.tunnelRotateCmd())
	return cmd
}

func (a *app) tunnelRotateCmd() *cobra.Command {
	var account string
	var yes bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Give a tunnel a new secret, so that no connector but pco's runs it",
		Long: "Give the tunnel of the install a new secret at Cloudflare and end the connections of all its\n" +
			"connectors. The connector pco runs on this node restarts with the new token at once; a\n" +
			"connector elsewhere, started with a token that was read with a stolen API token, loses its\n" +
			"session and cannot connect again. Do it when pco status names a connector that you do not\n" +
			"run. Only root may, and the question needs a terminal; a script passes --yes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			t, err := rotationTarget(st, account)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			describeRotation(s, st, t)
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Rotate the secret of tunnel %s? [y/N]", t.Name))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			res, err := a.client().RotateTunnel(ctx, t.AccountID)
			if err != nil {
				return a.explain(ctx, err)
			}
			s.printf("The secret of tunnel %s in account %s was rotated; its connector on this node restarts with the new token.\n"+
				"Follow it with pco status.\n", res.Tunnel, res.Account)
			return s.done()
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "the account of the tunnel; needed when the install has tunnels in several")
	addYesFlag(cmd, &yes)
	return cmd
}

// rotationTarget finds the tunnel a rotation is of in the state, as the
// daemon does: the one in account, or the only one.
func rotationTarget(st engine.State, account string) (engine.TunnelView, error) {
	var found []engine.TunnelView
	for _, t := range st.Tunnels {
		if t.Exists && t.ID != "" && !t.Unknown && (account == "" || t.AccountID == account) {
			found = append(found, t)
		}
	}
	switch {
	case len(found) == 0 && account != "":
		return engine.TunnelView{}, fmt.Errorf("no tunnel of this install is known in account %s; pco status lists the tunnels", account)
	case len(found) == 0:
		return engine.TunnelView{}, errors.New("no tunnel of this install is known; pco status lists the tunnels")
	case len(found) > 1:
		accounts := make([]string, 0, len(found))
		for _, t := range found {
			accounts = append(accounts, t.AccountID)
		}
		slices.Sort(accounts)
		return engine.TunnelView{}, fmt.Errorf("the install has tunnels in accounts %s and %s; name one with --account",
			strings.Join(accounts[:len(accounts)-1], ", "), accounts[len(accounts)-1])
	case found[0].Held != "":
		return engine.TunnelView{}, fmt.Errorf("tunnel %s in account %s is left as it is: %s; nothing was changed", found[0].Name, found[0].AccountID, found[0].Held)
	}
	return found[0], nil
}

// describeRotation says what a rotation does, and which connectors that pco
// does not run it would cut off.
func describeRotation(s *screen, st engine.State, t engine.TunnelView) {
	s.printf("This gives tunnel %s (%s) in account %s a new secret at Cloudflare and ends\n"+
		"the connections of every connector of it. The connector pco runs on this node restarts with\n"+
		"the new token; any other connector loses its session and cannot connect again.\n", t.Name, t.ID, t.AccountID)
	var rogue []string
	for _, r := range st.RogueConnectors {
		if r.TunnelID == t.ID {
			rogue = append(rogue, r.Text())
		}
	}
	if len(rogue) == 0 {
		return
	}
	s.println("Cloudflare lists connectors on it that pco does not run on this node:")
	for _, r := range rogue {
		s.printf("  - %s\n", r)
	}
}
