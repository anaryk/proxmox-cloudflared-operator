package main

import (
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
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
			t.row("  "+ownerText(v.Owner, v.Guest), dash(v.Identity), identityNow(v))
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
		s.printf("  %s\n", ownerText(g.String(), &g.GuestView))
	}
	return s.done()
}

// identityNow says whether a guest still has the identity it was approved in.
func identityNow(v engine.ApprovalView) string {
	switch {
	case v.Matches:
		return "the same"
	case v.Current == "":
		return "not in the last listing"
	}
	return "changed to " + v.Current + ": approve it again to publish it"
}

func (a *app) guestApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <owner>",
		Short: "Approve a guest in the identity it has now",
		Long: "Approve a guest, named as qemu/101 or lxc/200, in the identity the daemon sees it in now:\n" +
			"a guest re-created under the same VMID, or a clone, needs an approval of its own. The\n" +
			"daemon refuses a guest the last cycle did not see. An approval matters while the\n" +
			"admission mode is approve.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if _, err := a.client().ApproveGuest(cmd.Context(), args[0], ""); err != nil {
				return a.explain(cmd.Context(), err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Approved %s in the identity the daemon sees now; the next cycle publishes its routes.\n", args[0])
			return s.done()
		},
	}
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
			// Without an approval the daemon refuses, and says why.
			if i := slices.IndexFunc(approvals, func(v engine.ApprovalView) bool { return v.Owner == owner }); i >= 0 {
				v := approvals[i]
				s.printf("%s is approved in identity %s.\n", ownerText(v.Owner, v.Guest), v.Identity)
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
