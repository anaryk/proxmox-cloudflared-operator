package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// negativeTTL is how long an answer without addresses is kept when it came
// back quickly. A guest whose agent is not up yet is asked again soon; one
// whose agent hangs costs a call timeout per full ReportedTTL instead.
const negativeTTL = 15 * time.Second

// reportedEntry is the outcome of one interface lookup. A guest that could not
// answer is cached like any other result, as an entry without addresses.
type reportedEntry struct {
	identity  string // the guest it was read from
	expires   time.Time
	addrs     []model.ReportedAddr
	forbidden bool   // the token may not ask
	failure   string // problem line for an error nobody expects; empty otherwise
}

// reportedResult is the outcome of one interface fetch.
type reportedResult struct {
	addrs []model.ReportedAddr
	err   error
}

// refreshReported fills in the reported addresses of running watched guests,
// asking for those whose cached answer has expired. Anything else loses its
// cached answer, so that a guest that starts again is asked afresh. Guests on
// an offline node are not asked; their cached answer, if any, stays.
//
// No failure here clears Complete: a guest without usable addresses is a known
// state, and the addresses are a hint on top of the configured ones.
func (i *Inventory) refreshReported(ctx context.Context, r *run, ts []tracked) {
	var due []int
	for j := range ts {
		t := &ts[j]
		c := t.entry.reported
		switch {
		case !t.watched || !t.guest.Running:
			t.entry.reported = nil
		case t.offline:
		case c == nil || c.identity != t.guest.Identity || !r.now.Before(c.expires):
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
		c := ts[j].entry.reported
		if c == nil {
			continue
		}
		ts[j].guest.Reported = slices.Clone(c.addrs)
		forbidden = forbidden || c.forbidden
		if c.failure != "" {
			r.note("%s", c.failure)
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

// applyReported stores the outcome of a fetch together with how long it
// stays valid: a full ReportedTTL for addresses and for a timeout, negativeTTL
// (at most ReportedTTL) for any other answer without addresses. An error that
// is none of the expected ones is kept as a problem line for as long as the
// answer is cached.
func (i *Inventory) applyReported(r *run, t *tracked, res reportedResult) {
	entry := &reportedEntry{identity: t.guest.Identity}
	ttl := min(i.opts.ReportedTTL, negativeTTL)
	switch {
	case res.err == nil:
		entry.addrs = res.addrs
		if len(res.addrs) > 0 {
			ttl = i.opts.ReportedTTL
		}
	case errors.Is(res.err, pve.ErrAgentUnavailable):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("guest agent not available")
	case errors.Is(res.err, context.DeadlineExceeded):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("guest did not answer in time")
		ttl = i.opts.ReportedTTL
	case pve.IsForbidden(res.err):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("interfaces forbidden")
		entry.forbidden = true
	default:
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("reading interfaces failed")
		entry.failure = fmt.Sprintf("guest %s: interfaces not refreshed: %v", label(t.row), res.err)
	}
	entry.expires = r.now.Add(ttl)
	t.entry.reported = entry
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
