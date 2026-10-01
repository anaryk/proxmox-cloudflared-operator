package inventory

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// reportedEntry is the outcome of one interface lookup. An agent that could
// not answer is cached like any other result, as an entry without addresses,
// so that a guest whose agent hangs is not retried on every refresh.
type reportedEntry struct {
	identity  string // the guest it was read from
	at        time.Time
	addrs     []model.ReportedAddr
	forbidden bool // the token may not ask
}

// reportedResult is the outcome of one interface fetch.
type reportedResult struct {
	addrs []model.ReportedAddr
	err   error
}

// refreshReported fills in the reported addresses of running watched guests,
// asking for those whose cached answer has expired. Anything else loses its
// cached answer, so that a guest that starts again is asked afresh.
func (i *Inventory) refreshReported(ctx context.Context, r *run, ts []tracked) {
	var due []int
	for j := range ts {
		t := &ts[j]
		c := t.entry.reported
		switch {
		case !t.watched || !t.guest.Running:
			t.entry.reported = nil
		case c == nil || c.identity != t.guest.Identity || r.now.Sub(c.at) >= i.opts.ReportedTTL:
			due = append(due, j)
		}
	}
	results := make([]reportedResult, len(ts))
	i.forEach(ctx, len(due), func(k int) {
		results[due[k]] = i.fetchReported(ctx, ts[due[k]].row)
	})
	if ctx.Err() != nil {
		return
	}
	for _, j := range due {
		i.applyReported(r, &ts[j], results[j])
	}

	var forbidden bool
	for j := range ts {
		if c := ts[j].entry.reported; c != nil {
			ts[j].guest.Reported = slices.Clone(c.addrs)
			forbidden = forbidden || c.forbidden
		}
	}
	if forbidden {
		r.note("reported addresses unavailable: the API token lacks the guest-agent privilege (VM.GuestAgent.Audit, VM.Monitor before PVE 9)")
	}
	i.log.Debug().Int("interfaceCalls", len(due)).Msg("reported addresses refreshed")
}

// fetchReported asks a guest for its interfaces, giving up after callTimeout.
func (i *Inventory) fetchReported(ctx context.Context, row pve.Resource) reportedResult {
	ctx, cancel := context.WithTimeout(ctx, i.callTimeout)
	defer cancel()
	var (
		ifaces []pve.GuestIface
		err    error
	)
	if row.Kind == model.KindQEMU {
		ifaces, err = i.src.AgentInterfaces(ctx, row.Node, row.VMID)
	} else {
		ifaces, err = i.src.LXCInterfaces(ctx, row.Node, row.VMID)
	}
	if err != nil {
		return reportedResult{err: err}
	}
	return reportedResult{addrs: reportedAddrs(ifaces)}
}

// applyReported stores the outcome of a fetch. A guest that cannot answer
// because it has no agent, the agent hangs or the token may not ask has no
// reported addresses, which is a known state. Any other failure leaves the
// state unknown: the cached answer, if any, is kept and the snapshot is
// marked incomplete.
func (i *Inventory) applyReported(r *run, t *tracked, res reportedResult) {
	identity := t.guest.Identity
	switch {
	case res.err == nil:
		t.entry.reported = &reportedEntry{identity: identity, at: r.now, addrs: res.addrs}
	case errors.Is(res.err, pve.ErrAgentUnavailable), errors.Is(res.err, context.DeadlineExceeded):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("no interfaces reported")
		t.entry.reported = &reportedEntry{identity: identity, at: r.now}
	case pve.IsForbidden(res.err):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("interfaces forbidden")
		t.entry.reported = &reportedEntry{identity: identity, at: r.now, forbidden: true}
	default:
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("reading interfaces failed")
		r.fail("guest %s: interfaces not refreshed: %v", label(t.row), res.err)
		if c := t.entry.reported; c != nil && c.identity != identity {
			t.entry.reported = nil
		}
	}
}

// reportedAddrs flattens the interfaces of a guest into usable addresses.
// Container and VPN interfaces say nothing about where the guest itself can
// be reached, and loopback and link-local addresses cannot be reached from
// outside at all.
func reportedAddrs(ifaces []pve.GuestIface) []model.ReportedAddr {
	var out []model.ReportedAddr
	for _, ifc := range ifaces {
		if isContainerIface(ifc.Name) {
			continue
		}
		for _, a := range ifc.Addrs {
			if usable(a) {
				out = append(out, model.ReportedAddr{Iface: ifc.Name, MAC: ifc.MAC, Addr: a})
			}
		}
	}
	return out
}

// isContainerIface reports whether name starts with the name of a loopback,
// container-bridge or tunnel interface. The match is case-sensitive, as
// interface names are.
func isContainerIface(name string) bool {
	prefixes := []string{
		"lo", "docker", "br-", "veth", "cni", "cali", "flannel", "virbr",
		"lxcbr", "podman", "kube-", "tailscale", "wg",
	}
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

func usable(a netip.Addr) bool {
	return a.Is4() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsUnspecified() && !a.IsMulticast()
}
