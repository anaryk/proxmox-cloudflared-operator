package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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
// that fails.
func (a *attempt) verify(ctx context.Context, c Candidate) outcome {
	if why, denied := a.deny.Check(c.Addr); denied {
		return outcome{rejected, why}
	}
	ifaces, o := a.hostIfaces(ctx)
	if !o.ok() {
		return o
	}
	if isNodeAddr(ifaces, c.Addr) {
		return outcome{rejected, reasonNodeAddress}
	}
	// A MAC another running guest has too passes only on the forwarding
	// table, so there is nothing to probe when that cannot happen.
	if o := a.claimMAC(c.NIC.MAC, a.checksFDB()); !o.ok() {
		return o
	}
	if o := a.identify(ctx, ifaces, c); !o.ok() {
		return o
	}
	return a.r.dial(ctx, c.Addr, a.route.Target.Port)
}

// identify checks that only this guest answers for c.Addr on its own network.
func (a *attempt) identify(ctx context.Context, ifaces []HostIface, c Candidate) outcome {
	var macs []string
	var placed map[string]bool
	switch iface := arpInterface(ifaces, c); {
	case iface == "" && a.trusted(c):
	case iface == "":
		return lost("node has no address on %s in the guest's network", strings.Join(ifaceNames(c.NIC), " or "))
	default:
		own := a.ownMACs(c.NIC)
		var o outcome
		if macs, o = a.answeredBy(ctx, iface, c.Addr, own); !o.ok() {
			return o
		}
		if a.checksFDB() {
			if placed, o = a.forwarding(ctx, iface, c.NIC, macs, own); !o.ok() {
				return o
			}
		}
	}
	for _, mac := range append([]string{c.NIC.MAC}, macs...) {
		if o := a.claimMAC(mac, placed[normalMAC(mac)]); !o.ok() {
			return o
		}
	}
	return outcome{}
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

// checksFDB reports whether the forwarding-table step applies: on the node
// that hosts the guest, and whenever either node is not known.
func (a *attempt) checksFDB() bool {
	node, local := a.guest.Node, a.r.settings.LocalNode
	return node == "" || local == "" || node == local
}

// claimMAC fails when another running guest has mac configured too. Such a
// MAC passes only when the binding already had it and this call found it on
// the guest's own port.
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

// sharedWith returns another running guest that has mac configured.
func (a *attempt) sharedWith(mac string) (string, bool) {
	for _, g := range a.snap.Guests {
		if g.Ref == a.guest.Ref || !g.Running {
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
// admin allows it, and only inside the admin's ranges.
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

// forwarding checks that the bridge has learned each MAC on the port of the
// guest NIC that has it, and returns the MACs it placed there.
func (a *attempt) forwarding(ctx context.Context, iface string, nic model.NIC, macs []string, own map[string][]int) (map[string]bool, outcome) {
	bridge, vlan := nic.Bridge, nic.VLAN
	if nic.VLAN != 0 && iface == vlanBridge(nic) {
		bridge, vlan = iface, 0
	}
	placed := make(map[string]bool, len(macs))
	for _, mac := range macs {
		if ctx.Err() != nil {
			return nil, stopped()
		}
		port, found, err := a.r.prober.FDBPort(ctx, bridge, vlan, mac)
		if o := answered(ctx, err, "forwarding table of %s: %v", bridge); !o.ok() {
			return nil, o
		}
		switch {
		case !found:
			return nil, lost("MAC %s not seen on bridge %s", mac, bridge)
		case !slices.Contains(guestPorts(a.guest.Ref.VMID, own[mac]), port):
			return nil, lost("MAC %s is on port %s, not on the guest's own port", mac, port)
		}
		placed[mac] = true
	}
	return placed, outcome{}
}

// guestPorts names the bridge ports Proxmox creates for the guest NICs with
// the given indexes.
func guestPorts(vmid int, indexes []int) []string {
	out := make([]string, 0, 3*len(indexes))
	for _, n := range indexes {
		out = append(out,
			fmt.Sprintf("tap%di%d", vmid, n),
			fmt.Sprintf("fwpr%dp%d", vmid, n),
			fmt.Sprintf("veth%di%d", vmid, n),
		)
	}
	return out
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
