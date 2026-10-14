package main

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (a *app) networkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Attach pco to the networks of guests the node has no address in",
		Long: "A guest on a bridge or VLAN the node has an address in is reached directly. One on a bridge\n" +
			"or VLAN the node is not on is reached through a leg of pco's own, which attaching the network\n" +
			"puts there: a network card of the gateway namespace (on the host) or of the appliance, with\n" +
			"an address of the guests' network, and every target behind it published under a service\n" +
			"address of " + gateway.ServicePrefix.String() + ". Nothing is written to /etc/network/interfaces and the\n" +
			"guests are not touched. A bridge and VLAN to attach to is written as vmbr1 or vmbr1:20; the\n" +
			"network attached there has the id vmbr1 or vmbr1.20.",
		Example: "  # The bridges and VLANs a network may be attached to, with the guests seen there\n" +
			"  pco network candidates\n\n" +
			"  # Attach vmbr1 with the leg at 10.77.0.250\n" +
			"  pco network attach vmbr1 --address 10.77.0.250/24\n\n" +
			"  # The networks attached\n" +
			"  pco network list",
	}
	cmd.AddCommand(a.networkListCmd(), a.networkCandidatesCmd(), a.networkProbeCmd(), a.networkAttachCmd(),
		a.networkDetachCmd(), a.networkShowCmd(), newNetworkManagedCmd(a))
	return cmd
}

func (a *app) networkListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the networks attached, with their legs and the routes served through them",
		Long: "List the networks pco reaches through a leg: the id, the kind (attach, or managed for pco's\n" +
			"own network), the bridge, the VLAN, the prefix of the guests, the address of the leg on this\n" +
			"node, how the gateway last found the leg, and how many routes the last cycle served through\n" +
			"it.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # The networks attached\n" +
			"  pco network list\n\n" +
			"  # As JSON, for a script\n" +
			"  pco network list --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().NetworksRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			nets, err := a.client().Networks(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if len(nets) == 0 {
				s.println("No network is attached; pco network candidates lists the bridges and VLANs one may be attached to.")
				return s.done()
			}
			t := s.table()
			t.row("ID", "KIND", "BRIDGE", "VLAN", "PREFIX", "LEG", "STATE", "ROUTES")
			for _, n := range nets {
				leg, state := legOf(n)
				t.row(n.ID, n.Kind, n.Bridge, vlanText(n.VLAN), n.Prefix.String(), leg, state, strconv.Itoa(n.Routes))
			}
			t.flush()
			return s.done()
		},
	}
}

// legOf is the address of the leg of a network on this node, and how the
// gateway last found it: up, or why not. A network with legs on more nodes
// names the node of the first and how many more there are, and is up when
// every leg is.
func legOf(n engine.NetworkView) (addr, state string) {
	if len(n.Legs) == 0 {
		return "-", "no leg on this node"
	}
	l := n.Legs[0]
	if len(n.Legs) > 1 {
		addr := fmt.Sprintf("%s on %s and %d more", l.Addr, l.Node, len(n.Legs)-1)
		for _, other := range n.Legs {
			if _, state := legState(other); state != "up" {
				return addr, other.Node + ": " + state
			}
		}
		return addr, "up"
	}
	return legState(l)
}

// legState is the address of a leg and how its gateway last found it.
func legState(l engine.LegView) (addr, state string) {
	switch {
	case l.Up && l.Problem != "":
		return l.Addr.String(), "up, but " + l.Problem
	case l.Up:
		return l.Addr.String(), "up"
	}
	return l.Addr.String(), dash(l.Problem)
}

func vlanText(vlan int) string {
	if vlan == 0 {
		return "-"
	}
	return strconv.Itoa(vlan)
}

func (a *app) networkCandidatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "candidates",
		Short: "List the bridges and VLANs of the node a network may be attached to",
		Long: "List the bridges and SDN vnets of the node, and the VLANs of them that guests or the node\n" +
			"are on: whether one has a physical uplink, how many guest addresses are seen there, static\n" +
			"or reported, the prefix they suggest, and whether a network may be attached there: not\n" +
			"where the node has an address (it reaches the guests directly), not where one is attached\n" +
			"already, and not where the profile cannot, as pco's managed network or, in the appliance, a\n" +
			"tag on a bridge that does not filter VLANs. The daemon reads Proxmox and lists the guests\n" +
			"for it.\n\n" + jsonHelp + " It has every guest address by guest.\n\n" + askHelp,
		Example: "  # The bridges and VLANs of the node\n" +
			"  pco network candidates\n\n" +
			"  # With the guest addresses seen on each, as JSON\n" +
			"  pco network candidates --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().CandidateNetworksRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			candidates, err := a.client().CandidateNetworks(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			if len(candidates) == 0 {
				s.println("The node has no bridge or vnet.")
				return s.done()
			}
			t := s.table()
			t.row("SEGMENT", "KIND", "UPLINK", "GUESTS", "PREFIX", "STATE")
			for _, c := range candidates {
				prefix := "-"
				if len(c.Prefixes) > 0 {
					prefix = c.Prefixes[0].String()
				}
				t.row(engine.SegmentArg(c.Bridge, c.VLAN), candidateKind(c), yesNo(c.Uplink), strconv.Itoa(len(c.Guests)), prefix, candidateState(c))
			}
			t.flush()
			return s.done()
		},
	}
}

func candidateKind(c engine.CandidateNetwork) string {
	switch {
	case c.Vnet:
		return "vnet"
	case c.VLANAware:
		return "bridge, VLAN-aware"
	}
	return "bridge"
}

// candidateState says whether a network may be attached to a candidate, or
// why not.
func candidateState(c engine.CandidateNetwork) string {
	switch {
	case c.Refused != "":
		return "refused: " + c.Refused
	case c.Direct:
		return "direct: the node has an address here"
	case c.Attached != "":
		return "attached as " + c.Attached
	}
	return "can be attached"
}

func (a *app) networkProbeCmd() *cobra.Command {
	var (
		addr, prefix, scope, node string
		legFlags                  []string
	)
	cmd := &cobra.Command{
		Use:   "probe <bridge>[:<vlan>]",
		Short: "Ask who answers for an address on a bridge or VLAN",
		Long: "Probe an address on a bridge or VLAN the way the leg of a network checks its own before it\n" +
			"raises it: an ARP probe from no address, through the leg when the network is attached and\n" +
			"through a port of the gateway's own otherwise, which is seen on the LAN of a bridge with a\n" +
			"physical uplink while it probes. Without --prefix the prefix is the one most guest addresses\n" +
			"seen there are in, a /24 around them; without --address it is the highest address of the\n" +
			"prefix that no guest is known to have. --address may carry the length of the prefix, as\n" +
			"10.77.0.250/24. The appliance probes a bridge only through the card of a network attached\n" +
			"there; before that it says so, and the leg checks its address when it is raised. The command\n" +
			"exits with 1 when something answered or the address was not probed.\n\n" +
			"On a cluster --scope probes the legs of a network of that scope, as pco network attach does:\n" +
			"of scope cluster, this node's at --address and every other node's at the address --leg gives\n" +
			"or the highest free one, each by the agent of its node; of scope node, the leg of this node\n" +
			"or of the node --node names. A node without the bridge, or whose agent does not answer, is\n" +
			"shown without a leg and is no failure.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # Propose a free address on vmbr1\n" +
			"  pco network probe vmbr1\n\n" +
			"  # Is 10.20.0.250 free on VLAN 20 of vmbr2?\n" +
			"  pco network probe vmbr2:20 --address 10.20.0.250/24\n\n" +
			"  # On a cluster, the legs of VLAN 20 of vmbr2 on every node\n" +
			"  pco network probe vmbr2:20 --scope cluster",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			seg, err := parseSegment(args[0])
			if err != nil {
				return err
			}
			req, err := probeRequest(seg, addr, prefix)
			if err != nil {
				return err
			}
			legs, legPrefix, err := legsOf(legFlags)
			if err != nil {
				return err
			}
			if err := scopeFlags(scope, node, legs); err != nil {
				return err
			}
			req.Scope, req.Node, req.Legs = scope, node, legs
			if !req.Prefix.IsValid() {
				req.Prefix = legPrefix
			}
			ctx := cmd.Context()
			res, raw, err := a.client().ProbeNetwork(ctx, req)
			if err != nil {
				return a.explain(ctx, err)
			}
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			} else {
				s := &screen{w: cmd.OutOrStdout()}
				if scope != "" {
					printLegProbes(s, res)
				} else {
					printProbe(s, res)
				}
				if err := s.done(); err != nil {
					return err
				}
			}
			if scope != "" {
				if slices.ContainsFunc(res.Legs, func(l engine.LegProbe) bool { return !l.Missing && (l.Problem != "" || !l.Free) }) {
					return errReported
				}
				return nil
			}
			if !res.Free {
				return errReported
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "address", "", "the address to probe, as 10.77.0.250 or 10.77.0.250/24")
	cmd.Flags().StringVar(&prefix, "prefix", "", "the prefix of the guests' network, as 10.77.0.0/24")
	cmd.Flags().StringVar(&scope, "scope", "", "on a cluster, probe the legs of a network of scope cluster or node")
	cmd.Flags().StringVar(&node, "node", "", "the node of a network of --scope node, this one when not given")
	cmd.Flags().StringArrayVar(&legFlags, "leg", nil, "the leg of another node of a network of --scope cluster, as pve2=10.77.0.249 (repeatable)")
	return cmd
}

// probeRequest is a probe of an address on a segment, as the flags give the
// address, which may carry the length of the prefix, and the prefix.
func probeRequest(seg resolve.Segment, addr, prefix string) (engine.ProbeRequest, error) {
	req := engine.ProbeRequest{Bridge: seg.Bridge, VLAN: seg.VLAN}
	if prefix != "" {
		p, err := netip.ParsePrefix(prefix)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return req, fmt.Errorf("--prefix %q: want the IPv4 address of a network and its length, as 10.77.0.0/24", prefix)
		}
		req.Prefix = p
	}
	if addr == "" {
		return req, nil
	}
	if p, err := netip.ParsePrefix(addr); err == nil && p.Addr().Is4() {
		req.Address = p.Addr()
		if !req.Prefix.IsValid() {
			req.Prefix = p.Masked()
		}
		return req, nil
	}
	a, err := netip.ParseAddr(addr)
	if err != nil || !a.Is4() {
		return req, fmt.Errorf("--address %q: want an IPv4 address, as 10.77.0.250 or 10.77.0.250/24", addr)
	}
	req.Address = a
	return req, nil
}

// printProbe writes what a probe found: who answered, how the prefix and the
// address were chosen, the uplink the leg would be seen on and the route of
// the node the prefix overlaps.
func printProbe(s *screen, r engine.ProbeResult) {
	seg := resolve.Segment{Bridge: r.Bridge, VLAN: r.VLAN}
	switch {
	case r.NotProbed != "":
		s.printf("%s on %s, in %s, is not probed: %s.\n", r.Address, seg, r.Prefix, r.NotProbed)
	case r.Free:
		s.printf("%s on %s, in %s, is free: nothing answered the probe.\n", r.Address, seg, r.Prefix)
	default:
		s.printf("%s on %s, in %s, is in use: %s answered the probe.\n", r.Address, seg, r.Prefix, strings.Join(r.AnsweredBy, ", "))
	}
	if r.Note != "" {
		s.printf("  %s\n", r.Note)
	}
	if r.Warning != "" {
		s.printf("Uplink: %s has a physical uplink: %s.\n", seg, r.Warning)
	}
	if r.Overlap != "" {
		s.printf("Overlap: %s.\n", r.Overlap)
	}
}

func (a *app) networkAttachCmd() *cobra.Command {
	var (
		addr, scope, node        string
		legFlags                 []string
		confirmUplink, overlapOK bool
		yes                      bool
	)
	cmd := &cobra.Command{
		Use:   "attach <bridge>[:<vlan>] --address A/len",
		Short: "Attach a network: a leg of pco's own into a bridge or VLAN",
		Long: "Attach the network of a bridge or VLAN the node has no address in: a leg of pco's own with\n" +
			"the address --address gives, with the length of the guests' prefix, which nobody may hold.\n" +
			"The address is probed first and the result shown, then the question asked; a script passes\n" +
			"--yes. The appliance cannot probe a bridge before its card is there: it says so, and the leg\n" +
			"checks its address when the next cycle raises it and keeps it off if anyone answers. A\n" +
			"bridge with a physical uplink needs --confirm-uplink: the leg's MAC becomes visible\n" +
			"on that LAN, so the address has to be one reserved for pco there. A prefix that overlaps a\n" +
			"route of the node on another interface is refused unless --overlap-ok says that the guests\n" +
			"there are meant to be reached through the leg; the network records it. The first network\n" +
			"enables pco-netns.service. The next cycle raises the leg, and the routes of the guests\n" +
			"there are served through it from then on.\n\n" +
			"On a cluster the network has a scope, which the command asks for when --scope does not give\n" +
			"it, proposing the one the configuration of the bridge suggests. Of scope cluster, a bridge\n" +
			"with a physical uplink or a vnet of a VLAN, QinQ, VXLAN or EVPN zone, every node with the\n" +
			"bridge gets a leg: --address is this node's, --leg node=address names another node's, and\n" +
			"the others get the highest free address of the prefix. The agent of each node probes its\n" +
			"own and checks that the bridge is there; a node without the bridge, or whose agent does not\n" +
			"answer, gets no leg, and a later attach that names it adds one. Of scope node, an isolated\n" +
			"bridge or a vnet of a Simple zone, the network has one leg, on this node or the one --node\n" +
			"names, at --address.\n\n" + askHelp,
		Example: "  # Attach vmbr1 with the leg at 10.77.0.250\n" +
			"  pco network attach vmbr1 --address 10.77.0.250/24\n\n" +
			"  # VLAN 20 of a bridge with a physical uplink, from a script\n" +
			"  pco network attach vmbr2:20 --address 10.20.0.250/24 --confirm-uplink --yes\n\n" +
			"  # A network that overlaps a route of the node, on purpose\n" +
			"  pco network attach pcot1 --address 192.168.4.249/24 --overlap-ok\n\n" +
			"  # On a cluster, VLAN 20 of an uplinked bridge on every node, pve2 at an address of its own\n" +
			"  pco network attach vmbr2:20 --address 10.20.0.250/24 --scope cluster --leg pve2=10.20.0.249 --confirm-uplink\n\n" +
			"  # On a cluster, an isolated bridge of pve3\n" +
			"  pco network attach vmbr7 --address 10.70.0.250/24 --scope node --node pve3",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			seg, err := parseSegment(args[0])
			if err != nil {
				return err
			}
			legs, prefix, err := legsOf(legFlags)
			if err != nil {
				return err
			}
			if err := scopeFlags(scope, node, legs); err != nil {
				return err
			}
			var leg netip.Prefix
			switch {
			case addr != "":
				if leg, err = netip.ParsePrefix(addr); err != nil || !leg.Addr().Is4() {
					return fmt.Errorf("--address %q: want the address of the leg with the length of the guests' prefix, as 10.77.0.250/24", addr)
				}
				prefix = leg.Masked()
			case !prefix.IsValid():
				return errors.New("--address: give the address of the leg with the length of the guests' prefix, as 10.77.0.250/24")
			}
			at := attachArgs{seg: seg, arg: args[0], prefix: prefix, leg: leg, scope: scope, node: node, legs: legs,
				confirmUplink: confirmUplink, overlapOK: overlapOK, yes: yes}
			return a.attach(cmd, at)
		},
	}
	cmd.Flags().StringVar(&addr, "address", "", "the address of the leg with the length of the guests' prefix, as 10.77.0.250/24")
	cmd.Flags().BoolVar(&confirmUplink, "confirm-uplink", false, "attach to a bridge with a physical uplink, where the leg's MAC is seen on the LAN")
	cmd.Flags().BoolVar(&overlapOK, "overlap-ok", false, "attach a prefix that overlaps a route of the node on another interface")
	cmd.Flags().StringVar(&scope, "scope", "", "on a cluster, cluster for one segment on every node or node for a bridge of one node")
	cmd.Flags().StringVar(&node, "node", "", "the node of a network of --scope node, this one when not given")
	cmd.Flags().StringArrayVar(&legFlags, "leg", nil, "the leg of another node of a network of --scope cluster, as pve2=10.77.0.249 (repeatable)")
	addYesFlag(cmd, &yes)
	return cmd
}

// attachArgs are the flags of an attach as read.
type attachArgs struct {
	seg                      resolve.Segment
	arg                      string
	prefix                   netip.Prefix
	leg                      netip.Prefix // of this node, or of the node of --node
	scope, node              string
	legs                     map[string]netip.Addr
	confirmUplink, overlapOK bool
	yes                      bool
}

// attach probes the legs, shows what the probe found, asks, and attaches.
// On a cluster without a scope it asks for the scope first.
func (a *app) attach(cmd *cobra.Command, at attachArgs) error {
	ctx := cmd.Context()
	probe := func() (engine.ProbeResult, error) {
		res, _, err := a.client().ProbeNetwork(ctx, engine.ProbeRequest{Bridge: at.seg.Bridge, VLAN: at.seg.VLAN, Prefix: at.prefix,
			Address: at.leg.Addr(), Scope: at.scope, Node: at.node, Legs: at.legs})
		if err != nil {
			return res, a.explain(ctx, err)
		}
		return res, nil
	}
	res, err := probe()
	if err != nil {
		return err
	}
	if at.scope == "" && res.Clustered {
		if at.scope, err = a.askScope(cmd, at, res.Suggested); err != nil {
			return err
		}
		if res, err = probe(); err != nil {
			return err
		}
	}
	if at.scope != "" {
		return a.attachLegs(cmd, at, res)
	}
	if !at.leg.IsValid() {
		return errors.New("--address: give the address of the leg with the length of the guests' prefix, as 10.77.0.250/24")
	}
	s := &screen{w: cmd.OutOrStdout()}
	printProbe(s, res)
	if err := s.done(); err != nil {
		return err
	}
	switch {
	case !res.Free && res.NotProbed == "":
		return fmt.Errorf("%s is in use on %s; choose another address, which pco network probe %s proposes", at.leg.Addr(), at.seg, at.arg)
	case res.Uplink && !at.confirmUplink:
		return fmt.Errorf("%s has a physical uplink: %s; %s", at.seg, res.Warning, engine.UplinkHint)
	case res.Overlap != "" && !at.overlapOK:
		return fmt.Errorf("%s; %s", res.Overlap, engine.OverlapHint)
	}
	ok, err := a.confirm(cmd, at.yes, fmt.Sprintf("Attach network %s with the leg %s? [y/N]", store.SegmentID(at.seg.Bridge, at.seg.VLAN), at.leg))
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	v, err := a.client().Attach(ctx, engine.AttachRequest{
		Bridge: at.seg.Bridge, VLAN: at.seg.VLAN, Prefix: at.prefix, Address: at.leg.Addr(), ConfirmUplink: at.confirmUplink, OverlapOK: at.overlapOK,
	})
	if err != nil {
		return a.explain(ctx, err)
	}
	s.printf("Attached network %s: the leg %s with %s, mark %d. The next cycle raises it; pco network list shows how it is.\n",
		v.ID, gateway.LegIface(v.Mark), at.leg, v.Mark)
	return s.done()
}

// askScope asks the scope of a network on a cluster, the suggested one on
// an empty answer; a script names it with --scope.
func (a *app) askScope(cmd *cobra.Command, at attachArgs, suggested string) (string, error) {
	id := store.SegmentID(at.seg.Bridge, at.seg.VLAN)
	if _, terminal := a.stdinTerminal(cmd.InOrStdin()); at.yes || !terminal {
		return "", fmt.Errorf("%s is on a cluster: name the scope of the network with --scope cluster, one segment on every node, "+
			"or --scope node, a bridge of one node; %s is suggested", at.seg, suggested)
	}
	answer, err := a.ask(cmd, fmt.Sprintf("Scope of network %s: cluster, one segment on every node, or node, a bridge of one node? [cluster/node, empty for %s]", id, suggested))
	if err != nil {
		return "", err
	}
	scope := cmp.Or(strings.ToLower(answer), suggested)
	if scope != store.ScopeCluster && scope != store.ScopeNode {
		return "", fmt.Errorf("scope %q: want cluster or node", answer)
	}
	return scope, nil
}

// attachLegs shows the legs a probe of a scope found, asks, and attaches
// those of the nodes that may have one: a node without the bridge, or whose
// agent did not answer, gets none; any other problem, or an address in
// use, stops the attach.
func (a *app) attachLegs(cmd *cobra.Command, at attachArgs, res engine.ProbeResult) error {
	ctx := cmd.Context()
	s := &screen{w: cmd.OutOrStdout()}
	printLegProbes(s, res)
	if err := s.done(); err != nil {
		return err
	}
	chosen := map[string]netip.Addr{}
	uplink, overlap := false, ""
	var named []string
	for _, l := range res.Legs {
		switch {
		case l.Missing:
			continue
		case l.Problem != "":
			return fmt.Errorf("no leg on node %s: %s", l.Node, l.Problem)
		case !l.Free:
			return fmt.Errorf("%s is in use on %s of node %s; choose another address with --leg %s=<address>", l.Address, at.seg, l.Node, l.Node)
		}
		chosen[l.Node] = l.Address
		named = append(named, fmt.Sprintf("%s on %s", netip.PrefixFrom(l.Address, res.Prefix.Bits()), l.Node))
		uplink = uplink || l.Uplink
		overlap = cmp.Or(overlap, l.Overlap)
	}
	id := store.SegmentID(at.seg.Bridge, at.seg.VLAN)
	switch {
	case len(chosen) == 0:
		return fmt.Errorf("no node can have a leg of %s", id)
	case uplink && !at.confirmUplink:
		return fmt.Errorf("%s has a physical uplink: %s; %s", at.seg, cmp.Or(res.Warning, "the leg's MAC becomes visible on the LAN of "+at.seg.Bridge), engine.UplinkHint)
	case overlap != "" && !at.overlapOK:
		return fmt.Errorf("%s; %s", overlap, engine.OverlapHint)
	}
	legs := strings.Join(named, ", ")
	word := "legs"
	if len(chosen) == 1 {
		word = "leg"
	}
	ok, err := a.confirm(cmd, at.yes, fmt.Sprintf("Attach network %s of scope %s with the %s %s? [y/N]", id, at.scope, word, legs))
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	v, err := a.client().Attach(ctx, engine.AttachRequest{
		Bridge: at.seg.Bridge, VLAN: at.seg.VLAN, Prefix: res.Prefix, Scope: at.scope, Legs: chosen,
		ConfirmUplink: at.confirmUplink, OverlapOK: at.overlapOK,
	})
	if err != nil {
		return a.explain(ctx, err)
	}
	them := "them; pco network show " + v.ID + " shows how they are"
	if len(chosen) == 1 {
		them = "it; pco network show " + v.ID + " shows how it is"
	}
	s.printf("Attached network %s of scope %s: the %s %s with %s, mark %d. The next cycle raises %s.\n",
		v.ID, at.scope, word, gateway.LegIface(v.Mark), legs, v.Mark, them)
	return s.done()
}

// legsOf reads the --leg flags, node=address, and returns the prefix the
// first that gives the length of the prefix says.
func legsOf(flags []string) (map[string]netip.Addr, netip.Prefix, error) {
	var (
		legs   map[string]netip.Addr
		prefix netip.Prefix
	)
	for _, f := range flags {
		node, text, ok := strings.Cut(f, "=")
		if !ok || node == "" {
			return nil, prefix, fmt.Errorf("--leg %q: want the node and the address of its leg, as pve2=10.77.0.249", f)
		}
		a, err := netip.ParseAddr(text)
		if p, perr := netip.ParsePrefix(text); perr == nil {
			a, err = p.Addr(), nil
			if !prefix.IsValid() {
				prefix = p.Masked()
			}
		}
		if err != nil || !a.Is4() {
			return nil, prefix, fmt.Errorf("--leg %q: want an IPv4 address, as pve2=10.77.0.249 or pve2=10.77.0.249/24", f)
		}
		if legs == nil {
			legs = map[string]netip.Addr{}
		}
		if _, twice := legs[node]; twice {
			return nil, prefix, fmt.Errorf("--leg names node %s twice", node)
		}
		legs[node] = a
	}
	return legs, prefix, nil
}

// scopeFlags refuses a scope of another name, a node of a network that is
// not of scope node, and legs of a network that is not of scope cluster.
func scopeFlags(scope, node string, legs map[string]netip.Addr) error {
	switch {
	case scope != "" && scope != store.ScopeCluster && scope != store.ScopeNode:
		return fmt.Errorf("--scope %q: want cluster or node", scope)
	case node != "" && scope != store.ScopeNode:
		return errors.New("--node names the node of a network of --scope node")
	case len(legs) > 0 && scope != store.ScopeCluster:
		return errors.New("--leg names the legs of the other nodes of a network of --scope cluster")
	}
	return nil
}

// printLegProbes writes the legs a probe of a scope found, one for each node.
func printLegProbes(s *screen, r engine.ProbeResult) {
	seg := resolve.Segment{Bridge: r.Bridge, VLAN: r.VLAN}
	s.printf("Network %s of scope %s, in %s:\n", store.SegmentID(r.Bridge, r.VLAN), r.Scope, r.Prefix)
	uplink := false
	for _, l := range r.Legs {
		switch {
		case l.Problem != "":
			s.printf("  %s: no leg: %s\n", l.Node, l.Problem)
		case l.Free:
			s.printf("  %s: %s is free\n", l.Node, l.Address)
		default:
			s.printf("  %s: %s is in use: %s answered the probe\n", l.Node, l.Address, strings.Join(l.AnsweredBy, ", "))
		}
		if l.Overlap != "" {
			s.printf("    Overlap: %s.\n", l.Overlap)
		}
		uplink = uplink || l.Uplink
	}
	if r.Note != "" {
		s.printf("  %s\n", r.Note)
	}
	if uplink {
		s.printf("Uplink: %s has a physical uplink: %s.\n", seg, cmp.Or(r.Warning, "the leg's MAC becomes visible on the LAN of "+r.Bridge))
	}
}

func (a *app) networkDetachCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "detach <id>",
		Short: "Detach a network: remove its leg and its service addresses",
		Long: "Detach a network, named by its id as pco network list shows it (vmbr1, vmbr1.20): its leg is\n" +
			"removed at once, its service addresses are released, and the routes served through it are\n" +
			"no longer served that way. The routes served through it are shown first, and the question\n" +
			"needs a terminal; a script passes --yes. The last network detached disables\n" +
			"pco-netns.service, which takes nothing down: the namespace stays until pco is uninstalled.\n\n" + askHelp,
		Example: "  # Detach vmbr1, after a look at what it serves\n" +
			"  pco network detach vmbr1\n\n" +
			"  # VLAN 20 of vmbr2, from a script\n" +
			"  pco network detach vmbr2.20 --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			n, err := a.client().Network(ctx, args[0])
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			leg, _ := legOf(n.NetworkView)
			s.printf("Network %s: %s on %s, the leg at %s.\n", n.ID, n.Prefix, resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN}, leg)
			switch len(n.Served) {
			case 0:
				s.println("No route is served through it.")
			case 1:
				s.println("1 route is served through it:")
			default:
				s.printf("%d routes are served through it:\n", len(n.Served))
			}
			for _, host := range n.Served {
				s.printf("  %s\n", host)
			}
			s.printf("Detaching it removes the leg and releases %s; the routes through it are no longer served that way.\n",
				plural(len(n.Services), "service address", "service addresses"))
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Detach network %s? [y/N]", n.ID))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().Detach(ctx, n.ID, n.Rev, n.Served); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Detached network %s.\n", n.ID)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}

func plural(n int, one, more string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, more)
}

func (a *app) networkShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a network with its leg, its service addresses and the routes served through it",
		Long: "Show a network, named by its id as pco network list shows it: the bridge and VLAN, the\n" +
			"prefix, the leg on each node with how the gateway last found it, its mark and MAC, whether\n" +
			"the bridge has a physical uplink, overlap-ok when it was attached over a route of the node,\n" +
			"the service addresses the connectors dial for the guests behind it, and the routes served\n" +
			"through it.\n\n" + jsonHelp + "\n\n" + askHelp,
		Example: "  # The network of vmbr1\n" +
			"  pco network show vmbr1\n\n" +
			"  # VLAN 20 of vmbr2, as JSON\n" +
			"  pco network show vmbr2.20 --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().NetworkRaw(ctx, args[0])
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			n, err := a.client().Network(ctx, args[0])
			if err != nil {
				return a.explain(ctx, err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			a.printNetwork(s, n)
			return s.done()
		},
	}
}

// legMAC is the MAC of the leg of a network on a node as the daemon saved
// it; a node without one is said so.
func legMAC(n store.Network, node string) string {
	mac, err := n.LegMACOn(node, nil)
	if err != nil {
		return "not known"
	}
	return mac
}

func (a *app) printNetwork(s *screen, n engine.NetworkDetail) {
	s.printf("Network %s (%s)\n", n.ID, n.Kind)
	t := s.table()
	t.row("  bridge:", n.Bridge)
	t.row("  vlan:", vlanText(n.VLAN))
	t.row("  prefix:", n.Prefix.String())
	if n.Scope != "" {
		t.row("  scope:", n.Scope)
	}
	for _, l := range n.Legs {
		_, state := legState(l)
		t.row("  leg:", fmt.Sprintf("%s on %s, %s with MAC %s: %s", l.Addr, l.Node, gateway.LegIface(n.Mark), cmp.Or(l.MAC, legMAC(n.Network, l.Node)), state))
	}
	for _, node := range slices.Sorted(maps.Keys(n.Network.Legs)) {
		if !slices.ContainsFunc(n.Legs, func(l engine.LegView) bool { return l.Node == node }) {
			t.row("  leg:", fmt.Sprintf("%s on %s", n.Network.Legs[node], node))
		}
	}
	t.row("  mark:", strconv.Itoa(n.Mark))
	t.row("  uplink:", yesNo(n.Uplink))
	if n.OverlapOK {
		t.row("  overlap-ok:", "yes: attached over a route of the node")
	}
	t.row("  attached:", a.when(n.CreatedAt))
	t.row("  revision:", strconv.FormatInt(n.Rev, 10))
	t.flush()
	switch {
	case len(n.Services) == 0 && len(n.NodeServices) == 0:
		s.println("No service address: no target behind it is published.")
	case len(n.Services) > 0:
		s.println("Service addresses:")
		t = s.table()
		for _, m := range n.Services {
			t.row("  "+m.Service.String(), "-> "+m.Addr.String(), "since "+a.when(m.AllocatedAt))
		}
		t.flush()
	}
	for i, m := range n.NodeServices {
		if i == 0 || n.NodeServices[i-1].Node != m.Node {
			if i > 0 {
				t.flush()
			}
			s.printf("Service addresses on node %s:\n", m.Node)
			t = s.table()
		}
		t.row("  "+m.Service.String(), "-> "+m.Addr.String(), "since "+a.when(m.AllocatedAt))
		if i == len(n.NodeServices)-1 {
			t.flush()
		}
	}
	if len(n.Served) == 0 {
		s.println("No route is served through it.")
		return
	}
	s.println("Routes served through it:")
	for _, host := range n.Served {
		s.printf("  %s\n", host)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
