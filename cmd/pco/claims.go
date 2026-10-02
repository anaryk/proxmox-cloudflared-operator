package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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
		Long: "List who holds each public hostname, since when, and who waits for it. A claim is\n" +
			"serving when its holder publishes the hostname, conflict when others want it too, and\n" +
			"held when nobody serves it.\n\n" + jsonHelp,
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
			waiting[i] = ownerText(w.Owner, w.Guest)
		}
		note := ""
		if c.MissingSince != nil {
			note = "no longer asked for since " + a.when(*c.MissingSince)
		}
		t.row(c.Hostname, ownerText(c.Holder, c.Guest), dash(c.State), a.when(c.Since), dash(strings.Join(waiting, ", ")), dash(note))
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
			// Without a claim held by another owner the daemon refuses, and
			// says why.
			if i := slices.IndexFunc(claims, func(c engine.ClaimView) bool { return c.Hostname == host }); i >= 0 && claims[i].Holder != owner {
				describeMove(s, a, claims[i], owner)
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
			}
			if err := a.client().ResolveClaim(ctx, host, owner); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("The claim on %s is now held by %s; the next cycle publishes it.\n", host, owner)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// describeMove says who holds a hostname and what resolving it to owner does.
func describeMove(s *screen, a *app, c engine.ClaimView, owner string) {
	var guest *engine.GuestView
	if i := slices.IndexFunc(c.Waiting, func(w engine.ClaimantView) bool { return w.Owner == owner }); i >= 0 {
		guest = c.Waiting[i].Guest
	}
	what := "that route"
	if _, err := model.ParseGuestRef(owner); err == nil {
		what = "that guest"
	}
	holder := ownerText(c.Holder, c.Guest)
	s.printf("%s is held by %s since %s.\n", c.Hostname, holder, a.when(c.Since))
	s.printf("Resolving hands it to %s: the public hostname moves to %s, and %s waits for it.\n",
		ownerText(owner, guest), what, holder)
}

// ownerText names an owner, with the name of its guest when it has one:
// "qemu/101 (web-1)".
func ownerText(owner string, guest *engine.GuestView) string {
	if guest == nil || guest.Name == "" {
		return owner
	}
	return owner + " (" + guest.Name + ")"
}
