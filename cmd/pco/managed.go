package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func (a *app) managedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "managed",
		Short: "Consent to cards on the managed network, and see who has one",
		Long: "The managed network is an SDN vnet of pco's own, pconet, on which pco gives guests a card\n" +
			"with an address of managed.subnet, so that a guest on a network the node has no address in\n" +
			"is reached all the same. A guest gets a card when it consents: with the tag\n" +
			"cf-tunnel-managed, which only an admin may set while it is registered as admin-only, or\n" +
			"with an approval given here, in the identity the guest has then; with managed.consent at\n" +
			"auto also every guest with the gate tag that needs the network. The card goes after the\n" +
			"grace managed.detachGrace once the consent ends, and cf-tunnel-managed=off in the Notes of\n" +
			"a guest refuses it. pco network managed enable makes the network; pco guest approve is\n" +
			"another thing, the admission of a guest's routes.",
		Example: "  # The cards, the guests that wait and why, and the foreign cards\n" +
			"  pco managed status\n\n" +
			"  # Consent to a card for lxc/120 in the identity it has now\n" +
			"  pco managed approve lxc/120\n\n" +
			"  # Take the consent back; the card goes after the grace\n" +
			"  pco managed revoke lxc/120",
	}
	cmd.AddCommand(a.managedStatusCmd(), a.managedApproveCmd(), a.managedRevokeCmd())
	return cmd
}

func (a *app) managedStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the managed network: its cards, the guests that wait and the foreign cards",
		Long: "Show the managed network as the last cycle found it: whether it is enabled, the consent\n" +
			"mode and whether automatic attaches run, whether the vnet is as pco made it, the leg of this\n" +
			"node and the addresses left; the cards pco gave the guests, each with its state; the guests\n" +
			"that would get a card and why they do not, such as the gate not open yet, a grant the\n" +
			"appliance lacks (the pveum line is shown) or a firewall impact nobody confirmed; and the\n" +
			"cards on pconet no attachment explains, as a clone or a restore copies one, with what pco\n" +
			"did about each.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # The managed network\n" +
			"  pco managed status\n\n" +
			"  # The attachments as JSON\n" +
			"  pco managed status --json | jq '.attachments'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().ManagedRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			v, err := a.client().Managed(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.renderManaged(cmd.OutOrStdout(), v)
		},
	}
}

// managedSummary is the Managed line of pco status: the mode with why
// automatic attaches are stopped, and the counts.
func managedSummary(v engine.ManagedView) string {
	if !v.Enabled {
		return ""
	}
	mode := v.Consent
	if v.AutoStopped != "" {
		mode += " (stopped: " + v.AutoStopped + ")"
	}
	parts := []string{mode, plural(len(v.Attachments), "attached", "attached")}
	if n := len(v.Waiting); n > 0 {
		parts = append(parts, plural(n, "waiting", "waiting"))
	}
	if n := len(v.Foreign); n > 0 {
		parts = append(parts, plural(n, "foreign card", "foreign cards"))
	}
	if v.Refused != "" {
		parts = append(parts, "refused: "+v.Refused)
	} else if !v.Vnet.OK {
		parts = append(parts, "vnet changed: "+strings.Join(v.Vnet.Why, "; "))
	}
	return strings.Join(parts, ", ")
}

func (a *app) renderManaged(w io.Writer, v engine.ManagedView) error {
	s := &screen{w: w}
	if !v.Enabled {
		s.println("The managed network is not enabled; pco network managed enable makes it.")
		return s.done()
	}
	mode := v.Consent
	if v.AutoStopped != "" {
		mode += "; automatic attaches are stopped: " + v.AutoStopped
	}
	statusLine(s, "Mode", mode)
	tags := []string{}
	if v.TagHonoured {
		tags = append(tags, "cf-tunnel-managed is registered admin-only")
	} else {
		tags = append(tags, "cf-tunnel-managed is not registered admin-only, so it does not count")
	}
	statusLine(s, "Tags", strings.Join(tags, "; "))
	switch {
	case v.Refused != "":
		statusLine(s, "Refused", v.Refused)
	case v.Vnet.OK:
		statusLine(s, "Vnet", "pconet as pco made it")
	default:
		statusLine(s, "Vnet", "pconet changed outside pco: "+strings.Join(v.Vnet.Why, "; "))
	}
	statusLine(s, "Subnet", fmt.Sprintf("%s, %d addresses free", v.Subnet, v.Free))
	if v.Leg != nil {
		leg := v.Leg.Addr.String()
		switch {
		case v.Leg.Up && v.Leg.Problem != "":
			leg += ", up, but " + v.Leg.Problem
		case v.Leg.Up:
			leg += ", up"
		default:
			leg += ", " + dash(v.Leg.Problem)
		}
		statusLine(s, "Leg", leg)
	}
	if v.PoolGrant != "" {
		statusLine(s, "Pool grant", v.PoolGrant)
	}
	if v.Filtered != nil {
		statusLine(s, "Filtered", filteredText(v.Filtered))
	}

	if len(v.Attachments) == 0 {
		statusLine(s, "Attached", "none")
	} else {
		statusLine(s, "Attached", "")
		t := s.table()
		t.row("  GUEST", "NIC", "ADDRESS", "MAC", "CONSENT", "STATE", "SINCE")
		for _, att := range v.Attachments {
			t.row("  "+engine.OwnerName(att.Owner, att.Guest), "net"+strconv.Itoa(att.NIC), att.Addr.String(), att.MAC, att.Consent,
				att.State, a.when(att.Since))
		}
		t.flush()
		for _, att := range v.Attachments {
			switch {
			case att.GrantCmd != "":
				s.printf("  %s: %s\n", att.Owner, att.GrantCmd)
			case att.Detail != "":
				s.printf("  %s: %s\n", att.Owner, att.Detail)
			}
		}
	}
	if len(v.Waiting) > 0 {
		statusLine(s, "Waiting", "")
		for _, g := range v.Waiting {
			s.printf("  %s: %s\n", engine.OwnerName(g.String(), &g.GuestView), g.Why)
		}
	}
	if len(v.Foreign) > 0 {
		statusLine(s, "Foreign", "")
		for _, f := range v.Foreign {
			s.printf("  net%d (%s) of %s, %s: %s\n", f.NIC, f.MAC, engine.OwnerName(f.String(), &f.GuestView), f.Action, f.Why)
		}
	}
	return s.done()
}

// filteredText says whether the appliance's guests are isolated.
func filteredText(f *engine.FilteredView) string {
	switch {
	case !f.Offered:
		return "not offered on " + f.Backend
	case f.Holds:
		return "holds (" + f.Backend + ")"
	}
	return "does not hold (" + f.Backend + "): " + strings.Join(f.Why, "; ")
}

func (a *app) managedApproveCmd() *cobra.Command {
	var accept bool
	cmd := &cobra.Command{
		Use:   "approve <owner>",
		Short: "Consent to a card on the managed network for a guest",
		Long: "Consent to a card on the managed network for a guest, named as qemu/101 or lxc/120, in\n" +
			"the identity the daemon sees it in now: a guest re-created under the same VMID, or a\n" +
			"clone, needs a consent of its own. A guest that waits for the managed network is shown\n" +
			"first with why it waits. The daemon refuses the consent when the guest changed since it\n" +
			"was shown, and a guest the last cycle did not see. The card is added in the next cycle,\n" +
			"and goes after the grace once the consent is revoked.\n\n" +
			"In the appliance a guest whose other card has firewall=1 while its firewall is off waits\n" +
			"with needs-firewall-confirmation: turning its firewall on, which the appliance does for\n" +
			"the card it adds, filters that other card too. --accept-firewall-impact confirms that for\n" +
			"this guest; the setting managed.acceptFirewallImpact confirms it for every guest.\n\n" + askHelp,
		Example: "  # Consent to a card for lxc/120 in the identity it has now\n" +
			"  pco managed approve lxc/120\n\n" +
			"  # The same for a guest whose net0 has firewall=1, in the appliance\n" +
			"  pco managed approve lxc/120 --accept-firewall-impact",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			owner := args[0]
			v, err := a.client().Managed(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			identity := ""
			if i := slices.IndexFunc(v.Waiting, func(g engine.ManagedWaiting) bool { return g.String() == owner }); i >= 0 {
				shown := v.Waiting[i]
				identity = shown.Identity
				s.printf("%s waits for the managed network in identity %s: %s\n", engine.OwnerName(owner, &shown.GuestView), dash(shown.Identity), shown.Why)
			}
			consent, err := a.client().ApproveManaged(ctx, owner, identity, accept)
			if err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Approved %s for the managed network in identity %s.\n", consent.Owner, consent.Identity)
			if consent.AcceptFirewallImpact {
				s.println("The firewall-impact confirmation is recorded with it.")
			}
			s.println("The card is added in the next cycle; pco managed status shows it.")
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&accept, "accept-firewall-impact", false,
		"confirm that the appliance may turn the firewall of the guest on although another of its cards has firewall=1 "+
			"(a guest waiting with needs-firewall-confirmation)")
	return cmd
}

func (a *app) managedRevokeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke <owner>",
		Short: "Take back the consent of a guest; its card goes after the grace",
		Long: "Take back the consent given with pco managed approve. A card the guest has goes after\n" +
			"the grace managed.detachGrace, unless the tag cf-tunnel-managed or automatic consent keeps\n" +
			"it; consent back within the grace keeps the card. The question needs a terminal; a\n" +
			"script passes --yes.\n\n" + askHelp,
		Example: "  # Take back the consent of lxc/120\n" +
			"  pco managed revoke lxc/120\n\n" +
			"  # The same from a script, without the question\n" +
			"  pco managed revoke lxc/120 --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			owner := args[0]
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Revoking the consent of %s: a card it has goes after the grace, unless the tag or automatic consent keeps it.\n", owner)
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Revoke the consent of %s? [y/N]", owner))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().RevokeManaged(ctx, owner); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Revoked the consent of %s.\n", owner)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}
