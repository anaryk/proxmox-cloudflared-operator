package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

func (a *app) segmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "segment",
		Short: "Acknowledge the bridges and VLANs routes at observed may be served on",
		Long: "A route proven at the observed level is served only on a segment, a bridge and a VLAN, an\n" +
			"admin acknowledged: on a new one nothing is served until then. A segment is named as\n" +
			"vmbr1 for the untagged part of a bridge and as vmbr1:20 for VLAN 20 of it.",
		Example: "  # The segments routes at observed were proven on\n" +
			"  pco segment list\n\n" +
			"  # Serve the routes at observed on VLAN 20 of vmbr1\n" +
			"  pco segment acknowledge vmbr1:20",
	}
	cmd.AddCommand(a.segmentListCmd(), a.segmentAcknowledgeCmd(), a.segmentRevokeCmd())
	return cmd
}

func (a *app) segmentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the segments routes at observed were proven on, and those acknowledged",
		Long: "List the segments the last cycle proved routes at observed on, with how many, and those\n" +
			"acknowledged, with since when.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # The segments routes at observed were proven on\n" +
			"  pco segment list\n\n" +
			"  # As JSON, for a script\n" +
			"  pco segment list --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().SegmentsRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			segments, err := a.client().Segments(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if len(segments) == 0 {
				s.println("No route at observed was proven on a segment, and none is acknowledged.")
				return s.done()
			}
			t := s.table()
			t.row("SEGMENT", "ACKNOWLEDGED", "ROUTES")
			for _, v := range segments {
				acked := "no"
				if v.Acknowledged {
					acked = a.when(v.AcknowledgedAt)
				}
				t.row(engine.SegmentArg(v.Bridge, v.VLAN), acked, strconv.Itoa(v.Routes))
			}
			t.flush()
			return s.done()
		},
	}
}

func (a *app) segmentAcknowledgeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "acknowledge <bridge>[:<vlan>]",
		Short: "Serve the routes at observed on a segment",
		Long: "Acknowledge a segment: from the next cycle the routes at observed proven on it are served,\n" +
			"unless they wait for an approval of their guest. The routes it releases are shown first,\n" +
			"and the question needs a terminal; a script passes --yes.\n\n" + askHelp,
		Example: "  # Serve the routes at observed on the untagged part of vmbr1\n" +
			"  pco segment acknowledge vmbr1\n\n" +
			"  # On VLAN 20 of vmbr1, from a script\n" +
			"  pco segment acknowledge vmbr1:20 --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			seg, err := parseSegment(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			segments, err := a.client().Segments(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if v, ok := findSegment(segments, seg); ok && v.Acknowledged {
				s.printf("Segment %s is acknowledged since %s; there is nothing to do.\n", seg, a.when(v.AcknowledgedAt))
				return s.done()
			}
			st, err := a.state(ctx)
			if err != nil {
				return err
			}
			reason := engine.SegmentReason(seg.Bridge, seg.VLAN)
			waiting := slices.DeleteFunc(slices.Clone(st.Routes), func(r engine.RouteView) bool { return r.Reason != reason })
			switch len(waiting) {
			case 0:
				s.printf("No route waits for segment %s now.\n", seg)
			case 1:
				s.printf("1 route at observed waits for segment %s:\n", seg)
			default:
				s.printf("%d routes at observed wait for segment %s:\n", len(waiting), seg)
			}
			if len(waiting) > 0 {
				t := s.table()
				for _, r := range waiting {
					t.row("  "+r.Hostname, engine.OwnerName(r.Owner, r.Guest))
				}
				t.flush()
			}
			s.println("Acknowledging it serves the routes at observed proven on it, unless they wait for an approval of their guest.")
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Acknowledge segment %s? [y/N]", seg))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().AcknowledgeSegment(ctx, seg.Bridge, seg.VLAN); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Acknowledged segment %s; its routes at observed are served from the next cycle.\n", seg)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

func (a *app) segmentRevokeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke <bridge>[:<vlan>]",
		Short: "Take the acknowledgement of a segment back",
		Long: "Take the acknowledgement of a segment back: from the next cycle the routes at observed on\n" +
			"it are held until it is acknowledged again. The acknowledgement is shown first, and the\n" +
			"question needs a terminal; a script passes --yes.\n\n" + askHelp,
		Example: "  # Hold the routes at observed on VLAN 20 of vmbr1 again\n" +
			"  pco segment revoke vmbr1:20\n\n" +
			"  # The same from a script, without the question\n" +
			"  pco segment revoke vmbr1:20 --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			seg, err := parseSegment(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			segments, err := a.client().Segments(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			v, ok := findSegment(segments, seg)
			if !ok || !v.Acknowledged {
				s.printf("Segment %s is not acknowledged; there is nothing to revoke.\n", seg)
				return s.done()
			}
			proven := "1 route at observed was"
			if v.Routes != 1 {
				proven = fmt.Sprintf("%d routes at observed were", v.Routes)
			}
			s.printf("Segment %s is acknowledged since %s; %s proven on it in the last cycle.\n", seg, a.when(v.AcknowledgedAt), proven)
			s.println("Revoking it holds the routes at observed on it from the next cycle, until it is acknowledged again.")
			if err := s.done(); err != nil {
				return err
			}
			ok, err = a.confirm(cmd, yes, fmt.Sprintf("Revoke the acknowledgement of segment %s? [y/N]", seg))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().RevokeSegment(ctx, seg.Bridge, seg.VLAN); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Revoked the acknowledgement of segment %s.\n", seg)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

// parseSegment reads a segment as the commands take it: "vmbr1" or
// "vmbr1:20". What makes a bridge name is the daemon's to check.
func parseSegment(arg string) (resolve.Segment, error) {
	bad := fmt.Errorf("segment %q: want <bridge> or <bridge>:<vlan>, as vmbr1 or vmbr1:20, with a VLAN of 1 to 4094", arg)
	bridge, tag, tagged := strings.Cut(arg, ":")
	if bridge == "" {
		return resolve.Segment{}, bad
	}
	if !tagged {
		return resolve.Segment{Bridge: bridge}, nil
	}
	vlan, err := strconv.Atoi(tag)
	if err != nil || vlan < 1 || vlan > 4094 || strconv.Itoa(vlan) != tag {
		return resolve.Segment{}, bad
	}
	return resolve.Segment{Bridge: bridge, VLAN: vlan}, nil
}

func findSegment(segments []engine.SegmentView, seg resolve.Segment) (engine.SegmentView, bool) {
	i := slices.IndexFunc(segments, func(v engine.SegmentView) bool { return v.Bridge == seg.Bridge && v.VLAN == seg.VLAN })
	if i < 0 {
		return engine.SegmentView{}, false
	}
	return segments[i], true
}
