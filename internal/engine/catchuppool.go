package engine

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// problemReportRefused names what a node's report says its gateway maps that
// is not taken from it. It holds nothing: a report that is not taken is
// proof that the report is wrong, not that the address is in use.
const problemReportRefused = "node %s reports that its gateway maps %s, not taken from it: %s"

// maxCatchUp is the most the pool of service addresses moves on in an
// epoch, whatever moves it. An agent's word alone moves it once in an epoch,
// and no further than the store has mappings: a node that lies can make the
// pool skip that many addresses in an epoch, and no more.
const maxCatchUp = 1024

// maxReported is the most service addresses a node's report may name to be
// taken at all, and maxLeasesAgain the most leases catchUp saves again in a
// cycle: what a report or the inventory holds never grows the work of a
// cycle, or the store, without bound. The rest waits for the next cycle.
const (
	maxReported    = agentproto.MaxServiceAddrs
	maxLeasesAgain = 64
)

// catchUpMemory is what catchUp keeps of the epoch it last ran in; a new
// epoch begins it anew.
type catchUpMemory struct {
	epoch    agentproto.Epoch
	caughtUp bool
	// guessed says that the pool had its one move of the epoch on the
	// agents' word alone, and moved how far it moved in all.
	guessed bool
	moved   int
}

// of is the memory of epoch.
func (m *catchUpMemory) of(epoch agentproto.Epoch) *catchUpMemory {
	if m.epoch != epoch {
		*m = catchUpMemory{epoch: epoch}
	}
	return m
}

// raisePool moves the pool of service addresses on past the highest service
// address the controller can vouch for: one the store maps, or one a
// configuration verified at Cloudflare sends a connector to. An address only
// an agent's gateway maps moves it once in the epoch, once every node has
// reported (settled), and no further past the pool's next than the store has
// mappings. It says why nothing may be allocated now: the pool could not be
// read or moved, or would move more than maxCatchUp in the epoch. What a
// report names at or past the pool's next that is not taken from it is a
// problem line, and holds nothing.
func (c *cycleRun) raisePool(m *catchUpMemory, settled bool, overdue []string) string {
	next, _, _, err := c.e.d.Store.ServicePool()
	if err != nil {
		c.storeHold = true
		return fmt.Sprintf("reading the pool of service addresses: %v", err)
	}
	known, err := c.storedServices()
	if err != nil {
		c.storeHold = true
		return fmt.Sprintf("reading the service addresses: %v", err)
	}
	trusted := maps.Clone(known)
	for t := range c.e.sentTargets() {
		trusted[t.Addr.Unmap()] = true
	}
	want, highest := next, netip.Addr{}
	for addr := range trusted {
		if gateway.ServicePrefix.Contains(addr) && !addr.Less(want) {
			want, highest = addr.Next(), addr
		}
	}
	reported, refused := c.reportedServices(overdue)
	limit := addrPlus(next, len(known))
	for _, addr := range slices.SortedFunc(maps.Keys(reported), netip.Addr.Compare) {
		var why string
		switch {
		case addr.Less(next) || trusted[addr]:
			continue
		case m.guessed:
			why = "the pool moved on an agent's word once in this epoch already, and neither the store nor a configuration verified at Cloudflare has it"
		case !settled:
			// Taken, or not, once every node has reported.
			continue
		case !addr.Less(limit):
			why = fmt.Sprintf("an agent's word moves the pool at most %d past %s, as many addresses as the store has mappings", len(known), next)
		default:
			if !addr.Less(want) {
				want, highest = addr.Next(), addr
			}
			continue
		}
		refused = append(refused, refusedReport{node: reported[addr], addr: addr, why: why})
	}
	var why string
	if room := maxCatchUp - m.moved; addrDiff(want, next) > room {
		why = fmt.Sprintf("the pool of service addresses would move on more than %d in this epoch, to %s", maxCatchUp, want)
		want = addrPlus(next, room)
	}
	if want != next {
		if err := c.e.d.Store.SaveServicePool(want); err != nil {
			c.storeHold = true
			return fmt.Sprintf("moving the pool of service addresses on to %s: %v", want, err)
		}
		m.moved += addrDiff(want, next)
		c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindNetwork,
			Message: fmt.Sprintf("the pool of service addresses moved on from %s to %s: an agent's gateway or a configuration verified at Cloudflare holds %s",
				next, want, highest)})
	}
	if settled {
		m.guessed = true
	}
	for _, r := range refused {
		if r.addr.IsValid() && r.addr.Less(want) {
			continue
		}
		// The line is an event when it appears; it holds nothing.
		c.problem(problemReportRefused, r.node, r.what(), r.why)
	}
	return why
}

// storedServices are the service addresses the mappings of the store hold,
// of both kinds.
func (c *cycleRun) storedServices() (map[netip.Addr]bool, error) {
	out := map[netip.Addr]bool{}
	all, err := c.e.d.Store.ServiceAddrs()
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		out[m.Service.Unmap()] = true
	}
	nodes, err := c.e.d.Store.NodeServiceAddrs()
	if err != nil {
		return nil, err
	}
	for _, m := range nodes {
		out[m.Service.Unmap()] = true
	}
	return out, nil
}

// refusedReport is what a report names that is not taken from it: an
// address, or every address of a report that names too many.
type refusedReport struct {
	node string
	addr netip.Addr // zero for the whole report
	n    int
	why  string
}

func (r refusedReport) what() string {
	if r.addr.IsValid() {
		return r.addr.String()
	}
	return fmt.Sprintf("%d service addresses", r.n)
}

// reportedServices are the service addresses the agents of the admitted
// nodes report their gateways map, by the first node that reports each,
// each from a fresh report of the node its certificate names, and what is
// not taken whatever the pool: the addresses of a node that is not Active or
// Draining or of a report that names another node, and every one of a
// report that names more than maxReported. A report that is not fresh is not
// gone by, unless it is the last of an admitted node that allocation no
// longer waits for (overdue): what it maps stays taken.
func (c *cycleRun) reportedServices(overdue []string) (map[netip.Addr]string, []refusedReport) {
	out := map[netip.Addr]string{}
	var refused []refusedReport
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		rep := reports[name]
		late := slices.Contains(overdue, name)
		if !late && !c.fresh(rep) {
			continue
		}
		n, known := c.nodes[name]
		admitted := known && (n.Node.State == store.NodeActive || n.Node.State == store.NodeDraining ||
			late && n.Node.State == store.NodeUnknown && n.Node.Index != nil)
		if count := len(rep.Status.ServiceAddrs); count > maxReported {
			refused = append(refused, refusedReport{node: name, n: count, why: fmt.Sprintf("more than %d", maxReported)})
			continue
		}
		for _, addr := range rep.Status.ServiceAddrs {
			addr = addr.Unmap()
			if !gateway.ServicePrefix.Contains(addr) {
				continue
			}
			var why string
			switch {
			case !admitted:
				why = "the node is not admitted"
			case rep.Status.Node != name:
				why = fmt.Sprintf("its report names node %s", rep.Status.Node)
			default:
				if _, ok := out[addr]; !ok {
					out[addr] = name
				}
				continue
			}
			refused = append(refused, refusedReport{node: name, addr: addr, why: why})
		}
	}
	return out, refused
}

// fresh says that rep is recent enough to go by.
func (c *cycleRun) fresh(rep NodeReport) bool {
	return rep.Fresh && c.now.Sub(rep.ReceivedAt) < silentAfter
}

// addrPlus is the IPv4 address n past a.
func addrPlus(a netip.Addr, n int) netip.Addr {
	b := a.As4()
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], binary.BigEndian.Uint32(b[:])+uint32(n))
	return netip.AddrFrom4(out)
}

// addrDiff is how far the IPv4 address a is past b; zero when it is not.
func addrDiff(a, b netip.Addr) int {
	x, y := a.As4(), b.As4()
	ua, ub := binary.BigEndian.Uint32(x[:]), binary.BigEndian.Uint32(y[:])
	if ua < ub {
		return 0
	}
	return int(ua - ub)
}
