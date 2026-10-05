package main

import (
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (a *app) guestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guest",
		Short: "Approve guests for publishing, for admission mode approve",
	}
	cmd.AddCommand(a.guestListCmd(), a.guestApproveCmd(), a.guestRevokeCmd())
	return cmd
}

func (a *app) guestListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the approved guests and the guests that wait for approval",
		Long: "List the approved guests, each with the identity it was approved in and whether it still\n" +
			"has it, and the guests whose routes wait for an approval.\n\n" + jsonHelp + " It holds the\n" +
			"approvals; pco status --json has the guests that wait.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().ApprovalsRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			approvals, err := a.client().Approvals(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			return renderGuests(cmd.OutOrStdout(), approvals, st.Unapproved)
		},
	}
}

func renderGuests(w io.Writer, approvals []engine.ApprovalView, waiting []engine.UnapprovedGuest) error {
	s := &screen{w: w}
	if len(approvals) == 0 {
		s.println("No guest is approved.")
	} else {
		s.println("Approved guests:")
		t := s.table()
		t.row("  GUEST", "IDENTITY", "NOW")
		for _, v := range approvals {
			t.row("  "+engine.OwnerName(v.Owner, v.Guest), dash(v.Identity), present.IdentityNow(v))
		}
		t.flush()
		s.println("")
	}
	if len(waiting) == 0 {
		s.println("No guest waits for approval.")
		return s.done()
	}
	s.println("Waiting for approval (pco guest approve <owner>):")
	for _, g := range waiting {
		name := engine.OwnerName(g.String(), &g.GuestView)
		if len(g.Why) == 0 {
			s.printf("  %s\n", name)
			continue
		}
		s.printf("  %s: %s\n", name, strings.Join(g.Why, "; "))
	}
	return s.done()
}

func (a *app) guestApproveCmd() *cobra.Command {
	var allow []string
	cmd := &cobra.Command{
		Use:   "approve <owner>",
		Short: "Approve a guest in the identity it has now",
		Long: "Approve a guest, named as qemu/101 or lxc/200, in the identity the daemon sees it in now:\n" +
			"a guest re-created under the same VMID, or a clone, needs an approval of its own. A guest\n" +
			"that waits for approval is shown first, with the hostnames it would publish, why it waits\n" +
			"and what the approval records: at the observed level, the MACs its addresses answer from\n" +
			"and the soft-denied addresses, as the gateway of a node, it may be published at. The daemon\n" +
			"refuses the approval when the guest changed since it was shown, and a guest the last cycle\n" +
			"did not see. --allow-address allows an address the guest was not shown at. An approval\n" +
			"admits a guest while the admission mode is approve, and releases what waits at observed\n" +
			"in either mode.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			extra, err := allowedAddrs(allow)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			owner := args[0]
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			// What was shown is what is approved.
			var shown engine.UnapprovedGuest
			i := slices.IndexFunc(st.Unapproved, func(g engine.UnapprovedGuest) bool { return g.String() == owner })
			if i >= 0 {
				shown = st.Unapproved[i]
				s.printf("%s waits for approval in identity %s; approved, it publishes %s.\n",
					engine.OwnerName(owner, &shown.GuestView), dash(shown.Identity), dash(strings.Join(shown.Hostnames, ", ")))
				if len(shown.Why) > 0 {
					s.println("It waits because:")
					for _, why := range shown.Why {
						s.printf("  %s\n", why)
					}
				}
			}
			addrs := slices.Compact(slices.SortedFunc(slices.Values(slices.Concat(shown.Addresses, extra)), netip.Addr.Compare))
			if records := recordsText(shown.MACs, addrs); records != "" {
				s.printf("Approving it %s.\n", records)
			}
			approved, err := a.client().ApproveGuest(ctx, owner, shown.Identity, shown.MACs, addrs)
			if err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Approved %s in identity %s.\n", engine.OwnerName(approved.Owner, approved.Guest), approved.Identity)
			if approved.Mode == store.AdmissionApprove || i >= 0 || len(addrs) > 0 {
				s.println("From the next cycle its routes no longer wait for an approval.")
			} else {
				s.printf("The admission mode is %s: the approval matters only for its routes at observed until the mode is %s.\n",
					approved.Mode, store.AdmissionApprove)
			}
			return s.done()
		},
	}
	cmd.Flags().StringArrayVar(&allow, "allow-address", nil,
		"allow the guest to be published at this soft-denied address too (repeatable)")
	return cmd
}

// allowedAddrs reads the addresses of --allow-address.
func allowedAddrs(raw []string) ([]netip.Addr, error) {
	out := make([]netip.Addr, 0, len(raw))
	for _, r := range raw {
		addr, err := netip.ParseAddr(r)
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("--allow-address %q: want an IPv4 address", r)
		}
		out = append(out, addr)
	}
	return out, nil
}

// recordsText says what an approval records besides the identity: "records
// MAC m and allows address a", or nothing.
func recordsText(macs []string, addrs []netip.Addr) string {
	var parts []string
	switch len(macs) {
	case 0:
	case 1:
		parts = append(parts, "records MAC "+macs[0])
	default:
		parts = append(parts, "records MACs "+strings.Join(macs, ", "))
	}
	names := make([]string, len(addrs))
	for i, addr := range addrs {
		names[i] = addr.String()
	}
	switch len(names) {
	case 0:
	case 1:
		parts = append(parts, "allows address "+names[0])
	default:
		parts = append(parts, "allows addresses "+strings.Join(names, ", "))
	}
	return strings.Join(parts, " and ")
}

func (a *app) guestRevokeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke <owner>",
		Short: "Remove the approval of a guest",
		Long: "Remove the approval of a guest. While the admission mode is approve, its routes are no\n" +
			"longer published and its hostnames are held for it. The approval is shown first, and the\n" +
			"question needs a terminal; a script passes --yes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			owner := args[0]
			approvals, err := a.client().Approvals(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			i := slices.IndexFunc(approvals, func(v engine.ApprovalView) bool { return v.Owner == owner })
			if i < 0 {
				s.printf("%s has no approval; there is nothing to revoke.\n", owner)
				return s.done()
			}
			v := approvals[i]
			s.printf("%s is approved in identity %s.\n", engine.OwnerName(v.Owner, v.Guest), v.Identity)
			s.println("Revoking it stops its routes from being published while the admission mode is approve.")
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Revoke the approval of %s? [y/N]", owner))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().RevokeGuest(ctx, owner); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Revoked the approval of %s.\n", owner)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}
