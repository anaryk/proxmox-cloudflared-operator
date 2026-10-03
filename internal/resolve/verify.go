package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// verdict says how a candidate fared.
type verdict int

const (
	passed        verdict = iota
	rejected              // never to be served: denied, or an address of this node
	notIdentified         // the wire does not show the address belongs to this guest
	probeFailed           // the host could not be asked; proves nothing either way
	unreachable           // identity holds but the port does not answer
	cancelled             // the caller gave up; nothing was learned
)

type outcome struct {
	verdict verdict
	reason  string
}

func (o outcome) ok() bool { return o.verdict == passed }

func lost(format string, args ...any) outcome {
	return outcome{notIdentified, fmt.Sprintf(format, args...)}
}

func stopped() outcome { return outcome{cancelled, reasonCancelled} }

// answered checks a prober call that has returned. Once ctx has ended, its
// answer counts as not obtained, error or not, so that nothing learned after
// the caller gave up is taken as proof.
func answered(ctx context.Context, err error, format string, args ...any) outcome {
	switch {
	case ctx.Err() != nil:
		return stopped()
	case err != nil:
		return outcome{probeFailed, fmt.Sprintf(format, append(args, err)...)}
	}
	return outcome{}
}

// verify runs the checks on one candidate in order and stops at the first
// that fails. Once the identity holds, it returns what proves it, also when
// the port then fails.
func (a *attempt) verify(ctx context.Context, c Candidate) (proof, outcome) {
	if why, denied := a.forbidden(c.Addr); denied {
		return proof{}, outcome{rejected, why}
	}
	ifaces, o := a.hostIfaces(ctx)
	if !o.ok() {
		return proof{}, o
	}
	if isNodeAddr(ifaces, c.Addr) {
		return proof{}, outcome{rejected, reasonNodeAddress}
	}
	// A MAC that another guest which runs, or may, has too passes only on
	// the forwarding table, so there is nothing to probe when that cannot
	// happen.
	if o := a.claimMAC(c.NIC.MAC, a.checksFDB()); !o.ok() {
		return proof{}, o
	}
	p, o := a.identify(ctx, ifaces, c)
	if !o.ok() {
		return proof{}, o
	}
	return p, a.r.dial(ctx, c.Addr, a.route.Target.Port)
}

// identify checks that only this guest answers for c.Addr on its own network,
// and returns the level that proves it, with the bridge and the port the
// forwarding table placed the NIC's MAC on at level port.
func (a *attempt) identify(ctx context.Context, ifaces []HostIface, c Candidate) (proof, outcome) {
	var macs []string
	var placed map[string]string
	p := proof{level: LevelObserved}
	switch iface := arpInterface(ifaces, c); {
	case iface == "" && a.trusted(c):
		if o := a.throughGateway(ctx, c); !o.ok() {
			return proof{}, o
		}
	case iface == "":
		return proof{}, lost("node has no address on %s in the guest's network", strings.Join(ifaceNames(c.NIC), " or "))
	default:
		var o outcome
		if macs, placed, o = a.onWire(ctx, iface, c); !o.ok() {
			return proof{}, o
		}
		p.level = wireLevel(macs, placed)
		if p.level == LevelPort {
			p.bridge, _ = fdbOf(iface, c.NIC)
			p.port, p.ports = placed[normalMAC(c.NIC.MAC)], placed
		}
	}
	for _, mac := range append([]string{c.NIC.MAC}, macs...) {
		if o := a.claimMAC(mac, placed[normalMAC(mac)] != ""); !o.ok() {
			return proof{}, o
		}
	}
	return p, outcome{}
}

// wireLevel is port when the forwarding table placed every MAC that answered
// ARP on the guest's own port, and observed when it placed none, as for a
// guest on another node, whose MACs are only checked to be on no local
// guest's port.
func wireLevel(macs []string, placed map[string]string) Level {
	if len(macs) == 0 {
		return LevelObserved
	}
	for _, mac := range macs {
		if placed[mac] == "" {
			return LevelObserved
		}
	}
	return LevelPort
}

// onWire checks c.Addr on iface: the host's traffic for it leaves there, only
// this guest answers ARP for it, and the bridge has learned the MACs that
// answered where they belong. It returns those MACs and, by MAC, the guest's
// own port the forwarding table placed it on.
func (a *attempt) onWire(ctx context.Context, iface string, c Candidate) ([]string, map[string]string, outcome) {
	if o := a.followsRoute(ctx, iface, c.Addr); !o.ok() {
		return nil, nil, o
	}
	own := a.ownMACs(c.NIC)
	macs, o := a.answeredBy(ctx, iface, c.Addr, own)
	if !o.ok() {
		return nil, nil, o
	}
	if !a.checksFDB() {
		return macs, nil, a.notOnLocalPorts(ctx, iface, c.NIC, macs)
	}
	placed, o := a.forwarding(ctx, iface, c.NIC, macs, own)
	return macs, placed, o
}

func (a *attempt) hostIfaces(ctx context.Context) ([]HostIface, outcome) {
	if !a.ifacesRead {
		if ctx.Err() != nil {
			return nil, stopped()
		}
		a.ifaces, a.ifacesErr = a.r.prober.Interfaces(ctx)
		a.ifacesRead = true
	}
	if o := answered(ctx, a.ifacesErr, "listing host interfaces: %v"); !o.ok() {
		return nil, o
	}
	return a.ifaces, outcome{}
}

// isNodeAddr reports whether addr is configured on an interface of this
// node, which a guest must never get published whatever the denylist says.
func isNodeAddr(ifaces []HostIface, addr netip.Addr) bool {
	return slices.ContainsFunc(ifaces, func(ifc HostIface) bool {
		return slices.ContainsFunc(ifc.Addrs, func(p netip.Prefix) bool { return p.Addr() == addr })
	})
}

// isClusterNodeAddr reports whether addr is the cluster address of a node or
// configured on one of its interfaces, as far as the inventory knows. Like the
// host's own addresses, these are never published whatever the denylist says.
func isClusterNodeAddr(nodes []inventory.Node, addr netip.Addr) bool {
	return slices.ContainsFunc(nodes, func(n inventory.Node) bool {
		return n.Addr.Unmap() == addr || slices.ContainsFunc(n.Ifaces, func(ifc pve.NodeIface) bool {
			return slices.ContainsFunc(ifc.Addrs, func(p netip.Prefix) bool { return p.Addr().Unmap() == addr })
		})
	})
}

// forbidden says why addr may never be served for a guest: the denylist, or
// the address of a node.
func forbidden(deny Denylist, snap inventory.Snapshot, addr netip.Addr) (string, bool) {
	if why, denied := deny.Check(addr); denied {
		return why, true
	}
	if isClusterNodeAddr(snap.Nodes, addr) {
		return reasonClusterNode, true
	}
	return "", false
}

func (a *attempt) forbidden(addr netip.Addr) (string, bool) {
	return forbidden(a.deny, a.snap, addr)
}

// checksFDB reports whether the forwarding-table step applies: on the node
// that hosts the guest, and whenever it is not known that the guest runs on
// another node. A LocalNode that names no node of the inventory is a mistake
// in the settings, which must not switch the step off.
func (a *attempt) checksFDB() bool {
	node, local := a.guest.Node, a.r.settings.LocalNode
	listed := slices.ContainsFunc(a.snap.Nodes, func(n inventory.Node) bool { return n.Name == local })
	return node == "" || local == "" || node == local || !listed
}

// claimMAC fails when another guest that runs, or may run, has mac
// configured too. Such a MAC passes only when the binding already had it and
// this call found it on the guest's own port.
func (a *attempt) claimMAC(mac string, onOwnPort bool) outcome {
	key, err := model.NormalizeMAC(mac)
	if err != nil {
		return outcome{}
	}
	other, shared := a.sharedWith(key)
	if !shared || (onOwnPort && a.prev != nil && sameMAC(a.prev.MAC, key)) {
		return outcome{}
	}
	return lost("MAC %s is also configured on %s", key, other)
}

// sharedWith returns another guest that has mac configured and runs, or may
// run because Proxmox has never said whether it does.
func (a *attempt) sharedWith(mac string) (string, bool) {
	for _, g := range a.snap.Guests {
		if g.Ref == a.guest.Ref || !g.Running && !g.StatusUnknown {
			continue
		}
		if slices.ContainsFunc(g.NICs, func(n model.NIC) bool { return sameMAC(n.MAC, mac) }) {
			return g.Ref.String(), true
		}
	}
	return "", false
}

// arpInterface returns the host interface on the NIC's bridge and VLAN that
// has an address in the candidate's network, or "" when there is none.
func arpInterface(ifaces []HostIface, c Candidate) string {
	onLink := func(p netip.Prefix) bool { return p.Contains(c.Addr) }
	for _, name := range ifaceNames(c.NIC) {
		for _, ifc := range ifaces {
			if ifc.Name == name && slices.ContainsFunc(ifc.Addrs, onLink) {
				return name
			}
		}
	}
	return ""
}

// ifaceNames lists the host interfaces that sit in the NIC's network: the
// bridge when untagged; for a VLAN, its interface on a VLAN-aware bridge or
// the bridge Proxmox creates for it.
func ifaceNames(nic model.NIC) []string {
	if nic.VLAN == 0 {
		return []string{nic.Bridge}
	}
	return []string{fmt.Sprintf("%s.%d", nic.Bridge, nic.VLAN), vlanBridge(nic)}
}

func vlanBridge(nic model.NIC) string {
	return fmt.Sprintf("%sv%d", nic.Bridge, nic.VLAN)
}

// trusted reports whether a candidate the node has no address next to may be
// served anyway: only an address from the NIC's Proxmox config, only when the
// admin allows it, and only inside the admin's ranges. The route to it is
// checked as well; see throughGateway.
func (a *attempt) trusted(c Candidate) bool {
	s := a.r.settings
	inRange := func(p netip.Prefix) bool { return p.Contains(c.Addr) }
	return s.TrustStatic && slices.Contains(c.NIC.Static, c.Addr) && slices.ContainsFunc(s.TrustedCIDRs, inRange)
}

// ownMACs maps the MACs of this guest's NICs on nic's bridge and VLAN to the
// indexes of the NICs that have them.
func (a *attempt) ownMACs(nic model.NIC) map[string][]int {
	own := map[string][]int{}
	for _, n := range a.guest.NICs {
		if n.Bridge != nic.Bridge || n.VLAN != nic.VLAN {
			continue
		}
		if mac, err := model.NormalizeMAC(n.MAC); err == nil {
			own[mac] = append(own[mac], n.Index)
		}
	}
	return own
}

// answeredBy ARPs for addr and returns the MACs that answered, every one of
// which must be in own. More claimants than the prober collects is a failure
// of identity, not of the prober.
func (a *attempt) answeredBy(ctx context.Context, iface string, addr netip.Addr, own map[string][]int) ([]string, outcome) {
	if ctx.Err() != nil {
		return nil, stopped()
	}
	raw, err := a.r.prober.ARP(ctx, iface, addr)
	if ctx.Err() == nil && errors.Is(err, ErrTooManyClaimants) {
		return nil, lost("too many stations claim %s on %s", addr, iface)
	}
	if o := answered(ctx, err, "ARP on %s: %v", iface); !o.ok() {
		return nil, o
	}
	macs := normalizeMACs(raw)
	if len(macs) == 0 {
		return nil, lost("no ARP answer on %s", iface)
	}
	for _, mac := range macs {
		if _, ok := own[mac]; !ok {
			return nil, lost("%s answered by %s, which is not this guest", addr, mac)
		}
	}
	return macs, outcome{}
}

func (r *Resolver) dial(ctx context.Context, addr netip.Addr, port uint16) outcome {
	if ctx.Err() != nil {
		return stopped()
	}
	err := r.prober.Dial(ctx, netip.AddrPortFrom(addr, port))
	switch {
	case ctx.Err() != nil:
		return stopped()
	case err != nil:
		return outcome{unreachable, fmt.Sprintf("port %d: %v", port, err)}
	}
	return outcome{}
}

// normalizeMACs returns the distinct MACs in raw, in order. One that cannot be
// parsed is kept as it is, so that it is reported as a stranger.
func normalizeMACs(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if mac := normalMAC(s); !slices.Contains(out, mac) {
			out = append(out, mac)
		}
	}
	return out
}

// normalMAC returns s in the canonical form, or as it is when it is not a MAC.
func normalMAC(s string) string {
	if mac, err := model.NormalizeMAC(s); err == nil {
		return mac
	}
	return s
}

func sameMAC(a, b string) bool {
	x, errA := model.NormalizeMAC(a)
	y, errB := model.NormalizeMAC(b)
	return errA == nil && errB == nil && x == y
}
