package resolve

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// fdbOf returns the bridge and VLAN whose forwarding table holds the frames
// that reach iface from nic: the VLAN's own bridge has them untagged, a
// VLAN-aware bridge under the tag.
func fdbOf(iface string, nic model.NIC) (bridge string, vlan int) {
	if nic.VLAN != 0 && iface == vlanBridge(nic) {
		return iface, 0
	}
	return nic.Bridge, nic.VLAN
}

// forwarding checks that the bridge has learned each MAC on exactly one port,
// the port of the guest NIC that has it, and returns, by MAC, the port it
// placed it on. A MAC that proves nothing either way does not end the check:
// what the table says about the others can still show that the identity does
// not hold.
func (a *attempt) forwarding(ctx context.Context, iface string, nic model.NIC, macs []string, own map[string][]int) (map[string]string, outcome) {
	bridge, vlan := fdbOf(iface, nic)
	placed := make(map[string]string, len(macs))
	var unsure undecided
	for _, mac := range macs {
		ports, o := a.fdbPorts(ctx, bridge, vlan, mac)
		if o.ok() {
			o = a.onOwnPort(mac, bridge, ports, own[mac])
		}
		if !unsure.keep(o) {
			return nil, o
		}
		if o.ok() {
			placed[mac] = ports[0]
		}
	}
	if !unsure.ok() {
		return nil, unsure.outcome
	}
	return placed, outcome{}
}

// onOwnPort checks that ports, where the bridge has learned mac, is the port
// of a guest NIC with that MAC and nothing else. A guest said to run on
// another node while the inventory lists no node at all may well run there,
// so a failure then proves nothing, unless a port is a local guest's.
func (a *attempt) onOwnPort(mac, bridge string, ports []string, indexes []int) outcome {
	var o outcome
	switch {
	case len(ports) == 0:
		o = lost("MAC %s not seen on bridge %s", mac, bridge)
	case len(ports) > 1:
		o = lost("MAC %s is on several ports: %s", mac, strings.Join(ports, ", "))
	case !slices.Contains(GuestPorts(a.guest.Ref.VMID, indexes...), ports[0]):
		o = lost("MAC %s is on port %s, not on the guest's own port", mac, ports[0])
	default:
		return outcome{}
	}
	if a.mayRunElsewhere() && !slices.ContainsFunc(ports, isGuestPort) {
		return outcome{probeFailed, o.reason + " (no cluster nodes known)"}
	}
	return o
}

// mayRunElsewhere reports whether the guest is said to run on another node
// while the inventory lists no node at all, as when the listing failed right
// after a restart. That is missing information, unlike a LocalNode that names
// none of the nodes listed.
func (a *attempt) mayRunElsewhere() bool {
	node, local := a.guest.Node, a.r.settings.LocalNode
	return len(a.snap.Nodes) == 0 && node != "" && local != "" && node != local
}

// notOnLocalPorts checks, for a guest that runs on another node, that the
// bridge has learned none of the MACs that answered for it on the port of a
// guest of this node, which would be a local guest answering in its name.
func (a *attempt) notOnLocalPorts(ctx context.Context, iface string, nic model.NIC, macs []string) outcome {
	bridge, vlan := fdbOf(iface, nic)
	var unsure undecided
	for _, mac := range macs {
		ports, o := a.fdbPorts(ctx, bridge, vlan, mac)
		if i := slices.IndexFunc(ports, isGuestPort); o.ok() && i >= 0 {
			o = lost("MAC %s is on local port %s but the guest runs on %s", mac, ports[i], a.guest.Node)
		}
		if !unsure.keep(o) {
			return o
		}
	}
	return unsure.outcome
}

// undecided holds the first check of a series that proved nothing either way,
// to be reported only if no later check fails outright.
type undecided struct{ outcome }

// keep notes o and reports whether the series may go on: it may after a pass
// or a prober error, not after lost identity or a cancelled call.
func (u *undecided) keep(o outcome) bool {
	switch o.verdict {
	case passed:
		return true
	case probeFailed:
		if u.ok() {
			u.outcome = o
		}
		return true
	}
	return false
}

func (a *attempt) fdbPorts(ctx context.Context, bridge string, vlan int, mac string) ([]string, outcome) {
	if ctx.Err() != nil {
		return nil, stopped()
	}
	ports, err := a.r.prober.FDBPorts(ctx, bridge, vlan, mac)
	if o := answered(ctx, err, "forwarding table of %s: %v", bridge); !o.ok() {
		return nil, o
	}
	return ports, outcome{}
}

// GuestPorts names the bridge ports Proxmox creates for the NICs of a guest
// with the given indexes.
func GuestPorts(vmid int, indexes ...int) []string {
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

// isGuestPort reports whether name has the form of a bridge port Proxmox
// creates for a guest NIC: tap<vmid>i<n>, fwpr<vmid>p<n> or veth<vmid>i<n>.
func isGuestPort(name string) bool {
	for _, form := range [][2]string{{"tap", "i"}, {"fwpr", "p"}, {"veth", "i"}} {
		rest, ok := strings.CutPrefix(name, form[0])
		if !ok {
			continue
		}
		vmid, index, ok := strings.Cut(rest, form[1])
		if ok && isDigits(vmid) && isDigits(index) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
