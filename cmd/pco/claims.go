package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// jsonHelp is what the help of a command that prints an answer says of --json.
const jsonHelp = "With --json the answer of the daemon is printed as the daemon sent it, re-indented, with\n" +
	"control and bidirectional characters escaped."

func (a *app) claimsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claims",
		Short: "Show who holds each public hostname, and hand one to another owner",
	}
	cmd.AddCommand(a.claimsListCmd(), a.claimsResolveCmd())
	return cmd
}

func (a *app) claimsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the claims on the public hostnames",
		Long: "List who holds each public hostname, since when, and who waits for it. As the last cycle\n" +
			"that settled the claims found it, a claim is serving when its holder publishes the\n" +
			"hostname, conflict when others want it too, held when nobody serves it, and pending when\n" +
			"another owner served it, as after a resolve. It is unknown until a cycle of the daemon\n" +
			"has settled the claims.\n\n" + jsonHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().ClaimsRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			claims, err := a.client().Claims(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.renderClaims(cmd.OutOrStdout(), claims)
		},
	}
}

func (a *app) renderClaims(w io.Writer, claims []engine.ClaimView) error {
	s := &screen{w: w}
	if len(claims) == 0 {
		s.println("No claims.")
		return s.done()
	}
	t := s.table()
	t.row("HOSTNAME", "HOLDER", "STATE", "SINCE", "WAITING", "NOTE")
	for _, c := range claims {
		waiting := make([]string, len(c.Waiting))
		for i, w := range c.Waiting {
			waiting[i] = engine.OwnerName(w.Owner, w.Guest)
		}
		note := ""
		if c.MissingSince != nil {
			note = "no longer asked for since " + a.when(*c.MissingSince)
		}
		t.row(c.Hostname, engine.OwnerName(c.Holder, c.Guest), dash(c.State), a.when(c.Since), dash(strings.Join(waiting, ", ")), dash(note))
	}
	t.flush()
	return s.done()
}

func (a *app) claimsResolveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "resolve <hostname> <owner>",
		Short: "Hand a public hostname to another owner that claims it",
		Long: "Move the claim on a hostname from its holder to another owner that claims it: a guest,\n" +
			"as qemu/102, or a manual route, as manual/<id>. The public hostname goes to that owner,\n" +
			"and the holder waits for it as every other claimant does. Who holds it and who would are\n" +
			"shown first, and the question needs a terminal; a script passes --yes. The daemon refuses\n" +
			"an owner that does not claim the hostname.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			host, err := hostname.Normalize(args[0])
			if err != nil {
				return fmt.Errorf("%q is not a hostname: %w", args[0], err)
			}
			owner := args[1]
			claims, err := a.client().Claims(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			i := slices.IndexFunc(claims, func(c engine.ClaimView) bool { return c.Hostname == host })
			switch {
			case i < 0:
				s.printf("Nobody holds a claim on %s; there is nothing to move.\n", host)
				return s.done()
			case claims[i].Holder == owner:
				s.printf("%s holds %s already; there is nothing to move.\n", engine.OwnerName(owner, claims[i].Guest), host)
				return s.done()
			}
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			describeMove(s, a, claims[i], owner, st)
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Move %s to %s? [y/N]", host, owner))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			// An owner that does not claim the hostname the daemon refuses,
			// and says why.
			if err := a.client().ResolveClaim(ctx, host, owner); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("The claim on %s is now held by %s.\n", host, owner)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// describeMove says who holds a hostname, who would, and what that brings
// about as far as the state tells.
func describeMove(s *screen, a *app, c engine.ClaimView, owner string, st engine.State) {
	var guest *engine.GuestView
	if i := slices.IndexFunc(c.Waiting, func(w engine.ClaimantView) bool { return w.Owner == owner }); i >= 0 {
		guest = c.Waiting[i].Guest
	}
	holder := engine.OwnerName(c.Holder, c.Guest)
	s.printf("%s is held by %s since %s.\n", c.Hostname, holder, a.when(c.Since))
	s.printf("Resolving hands it to %s; %s waits for it from then on, in the place in line its claim gives it.\n",
		engine.OwnerName(owner, guest), holder)
	for _, line := range moveOutcome(c.Hostname, owner, st) {
		s.println(line)
	}
}

// moveOutcome says when the new holder of a hostname will serve it: not
// while it waits for approval or names the hostname without a route for it,
// and nothing is published while the daemon only observes or holds; otherwise
// it serves it once a cycle has verified its address.
func moveOutcome(host, owner string, st engine.State) []string {
	var out []string
	waits := slices.ContainsFunc(st.Unapproved, func(g engine.UnapprovedGuest) bool {
		return g.String() == owner && slices.Contains(g.Hostnames, host)
	})
	routed := slices.ContainsFunc(st.Routes, func(r engine.RouteView) bool { return r.Hostname == host && r.Owner == owner })
	switch {
	case waits:
		out = append(out, fmt.Sprintf("%s waits for approval: nobody serves %s until it is approved (pco guest approve %s).", owner, host, owner))
	case !routed:
		out = append(out, fmt.Sprintf("%s names %s without a route for it: nobody serves it until %s routes it.", owner, host, owner))
	}
	if st.Mode == engine.ModeObserve {
		out = append(out, "The daemon only observes: the claim moves now, but nothing is published until pco apply.")
	}
	if st.Hold != "" {
		out = append(out, fmt.Sprintf("The daemon holds (%s): the claim moves now, but %s serves %s only once the daemon stops holding.",
			st.Hold, owner, host))
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("From the next cycle %s holds it, and serves it once its address is verified: "+
			"pco diagnose %s shows how that goes.", owner, host))
	}
	return out
}
