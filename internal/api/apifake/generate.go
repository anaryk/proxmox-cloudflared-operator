package apifake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The reasons of routes that are not served, as the planner gives them.
const (
	reasonNotAnswering = "target is not answering"
	reasonNoAddr       = "no verified address yet"
	reasonOtherOwner   = "address was verified for another owner"
	reasonWithdrawn    = "identity check failed"
	reasonNotServed    = "zone %s is served through no credential"
)

// generate makes a scenario from populated, whose routes, tunnels and zones
// are the patterns of what it adds. What it makes depends on nothing else.
func generate(kind string, base files) (files, error) {
	m, err := newMaker(base)
	if err != nil {
		return files{}, err
	}
	switch kind {
	case "large":
		m.large()
	case "outage":
		m.outage()
	case "wide":
		m.wide()
	default:
		return files{}, fmt.Errorf("there is no generator %q; there are large, outage and wide", kind)
	}
	m.finish()
	return m.f, nil
}

// maker adds to populated, which is its own to change.
type maker struct {
	f       files
	route   engine.RouteView // an active route with a path
	zone    engine.ZoneView  // a zone that is served
	tunnel  engine.TunnelView
	conn    connector.Status
	traffic engine.TunnelTraffic
	guests  int // how many guests the routes added were given
}

func newMaker(base files) (*maker, error) {
	m := &maker{f: base}
	st := &m.f.State
	i := slices.IndexFunc(st.Routes, func(r engine.RouteView) bool { return r.State == planner.StateActive && r.Path != nil })
	j := slices.IndexFunc(st.Zones, func(z engine.ZoneView) bool { return z.State == engine.ZoneServed })
	if i < 0 || j < 0 || len(st.Connectors) == 0 || m.f.Traffic == nil || len(m.f.Traffic.Tunnels) == 0 {
		return nil, fmt.Errorf("populated has no active route with a path, served zone, connector or traffic to make a scenario of")
	}
	m.route, m.zone, m.conn, m.traffic = st.Routes[i], st.Zones[j], st.Connectors[0], m.f.Traffic.Tunnels[0]
	k := slices.IndexFunc(st.Tunnels, func(t engine.TunnelView) bool { return t.ID == m.conn.TunnelID })
	if k < 0 {
		return nil, fmt.Errorf("populated has no tunnel for its connector")
	}
	m.tunnel = st.Tunnels[k]
	return m, nil
}

// large is 1000 routes over 6 zones, five served and one frozen, 40 of them
// not active, and 2000 guests.
func (m *maker) large() {
	served := []engine.ZoneView{m.zone}
	for _, name := range []string{"example.app", "example.dev", "example.biz", "example.eu"} {
		served = append(served, m.addZone(name, m.zone.AccountID))
	}
	frozen := m.zoneNamed("example.info")
	unserved := m.zoneNamed("example.net")
	extra := 40 - m.notActive()
	n := 1000 - len(m.f.State.Routes)
	for i := range n {
		zone := served[i%len(served)]
		switch {
		case i >= extra:
			m.add(m.active(i, zone))
		case i%5 < 2:
			m.add(down(m.active(i, zone), planner.StateUnreachable, reasonNotAnswering))
		case i%5 == 2:
			m.add(down(m.active(i, zone), planner.StateWithdrawn, reasonWithdrawn))
		case i%5 == 3:
			m.add(frozenIn(m.active(i, frozen), frozen))
		default:
			m.add(noZone(m.active(i, unserved), fmt.Sprintf(reasonNotServed, unserved.Name)))
		}
	}
	m.fillGuests(2000)
	m.setRate(m.traffic.TunnelID, 420.5, 0.8)
}

// outage is 1000 routes, 600 of them unreachable for one of three reasons.
func (m *maker) outage() {
	served := []engine.ZoneView{m.zone, m.addZone("example.app", m.zone.AccountID), m.addZone("example.dev", m.zone.AccountID)}
	reasons := []string{reasonNotAnswering, reasonNoAddr, reasonOtherOwner}
	down600 := 600 - m.count(planner.StateUnreachable)
	n := 1000 - len(m.f.State.Routes)
	for i := range n {
		r := m.active(i, served[i%len(served)])
		if i < down600 {
			r = down(r, planner.StateUnreachable, reasons[i%len(reasons)])
		}
		m.add(r)
	}
	m.setRate(m.traffic.TunnelID, 96.4, 31.2)
}

// wide is 50 zones in 30 accounts, so 30 tunnels, and 300 routes. Every
// account has a tunnel of the install, so the accounts added are counted by
// the tunnels.
func (m *maker) wide() {
	var accounts []string
	var zones []engine.ZoneView
	for _, tv := range m.f.State.Tunnels {
		if !slices.ContainsFunc(m.f.State.Zones, func(z engine.ZoneView) bool { return z.AccountID == tv.AccountID }) {
			zones = append(zones, m.addZone(fmt.Sprintf("%s.example.com", tv.AccountID), tv.AccountID))
		}
	}
	for a := len(m.f.State.Tunnels); a < 30; a++ {
		account := hexID("account", a)
		accounts = append(accounts, account)
		m.addAccount(account, fmt.Sprintf("Team %02d", a+1), a)
		zones = append(zones, m.addZone(fmt.Sprintf("team%02d.example.com", a+1), account))
	}
	for i := 0; len(m.f.State.Zones) < 50; i++ {
		zones = append(zones, m.addZone(fmt.Sprintf("team%02d.example.net", i+1), accounts[i%len(accounts)]))
	}
	n := 300 - len(m.f.State.Routes)
	for i := range n {
		m.add(m.active(i, zones[i%len(zones)]))
	}
}

func (m *maker) notActive() int {
	return len(m.f.State.Routes) - m.count(planner.StateActive)
}

func (m *maker) count(state planner.RouteState) int {
	n := 0
	for _, r := range m.f.State.Routes {
		if r.State == state {
			n++
		}
	}
	return n
}

func (m *maker) zoneNamed(name string) engine.ZoneView {
	i := slices.IndexFunc(m.f.State.Zones, func(z engine.ZoneView) bool { return z.Name == name })
	if i < 0 {
		return m.addZone(name, m.zone.AccountID)
	}
	return m.f.State.Zones[i]
}

// addZone adds a zone served as the pattern zone is, in account.
func (m *maker) addZone(name, account string) engine.ZoneView {
	z := m.zone
	z.Name, z.ID, z.AccountID, z.Pinned = name, hexID("zone", len(m.f.State.Zones)), account, ""
	m.f.State.Zones = append(m.f.State.Zones, z)
	return z
}

// addAccount adds an account with a tunnel of the install, its connector on
// the node and their traffic; n numbers it.
func (m *maker) addAccount(account, name string, n int) {
	st := &m.f.State
	t := m.tunnel
	t.AccountID, t.ID, t.Version = account, fmt.Sprintf("00000000-0000-4000-8000-%012d", 100+n), 1+n%7
	if t.Rollout != nil {
		rollout := *t.Rollout
		rollout.Version = t.Version
		t.Rollout = &rollout
	}
	st.Tunnels = append(st.Tunnels, t)
	c := m.conn
	c.TunnelID, c.ConnectorID, c.MetricsAddr = t.ID, fmt.Sprintf("6b1f0e4c-29a4-4c43-9d2c-%012d", 100+n), fmt.Sprintf("127.0.0.1:%d", 20300+n)
	st.Connectors = append(st.Connectors, c)
	tt := m.traffic
	tt.TunnelID, tt.ConfigVersion, tt.Samples = t.ID, t.Version, slices.Clone(m.traffic.Samples)
	for i := range tt.Samples {
		tt.Samples[i].RPS, tt.Samples[i].ErrorsPerSec = float64(n%9)*3.5, 0
	}
	m.f.Traffic.Tunnels = append(m.f.Traffic.Tunnels, tt)
	for i := range st.Credentials {
		if st.Credentials[i].ID == t.CredentialID && st.Credentials[i].Checked {
			r := &st.Credentials[i].Report
			r.Accounts = append(r.Accounts, cfapi.Account{ID: account, Name: name})
		}
	}
}

// active is a route of a guest of its own, numbered n, in zone.
func (m *maker) active(n int, zone engine.ZoneView) engine.RouteView {
	r := m.route
	path := *m.route.Path
	r.Path = &path
	vmid := 1000 + n
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: vmid}
	addr := netip.AddrFrom4([4]byte{10, 1, byte(n / 250), byte(n%250 + 1)})
	service := fmt.Sprintf("http://%s:8080", addr)
	host := fmt.Sprintf("app-%04d.%s", n, zone.Name)
	r.Hostname, r.Owner, r.State, r.Reason, r.Service, r.Zone, r.Warnings = host, ref.String(), planner.StateActive, "", service, zone.Name, nil
	r.Level, r.Account = string(resolve.LevelPort), zone.AccountID
	r.Guest = &engine.GuestView{GuestRef: ref, Name: fmt.Sprintf("app-%04d", n)}
	r.Candidates = []resolve.CandidateResult{{Addr: addr, Source: resolve.FromStatic, OK: true, Level: string(resolve.LevelPort)}}
	r.Rule = &planner.IngressRule{Hostname: host, Service: service}
	r.Path.Bridge, r.Path.VLAN = "vmbr0", 0
	if n%4 == 3 {
		r.Path.Bridge, r.Path.VLAN = "vmbr1", 20
	}
	r.Path.Port, r.Path.MAC = fmt.Sprintf("tap%di0", vmid), fmt.Sprintf("bc:24:11:%02x:%02x:%02x", byte(vmid>>16), byte(vmid>>8), byte(vmid))
	return r
}

// down is r not served: its rule answers 503, and its address did not pass.
func down(r engine.RouteView, state planner.RouteState, reason string) engine.RouteView {
	r.State, r.Reason, r.Service, r.Level = state, reason, "", ""
	r.Rule.Service = planner.BlockedService
	for i := range r.Candidates {
		r.Candidates[i].OK, r.Candidates[i].Reason, r.Candidates[i].Level = false, reason, ""
	}
	return r
}

// frozenIn is r in a zone whose account is frozen.
func frozenIn(r engine.RouteView, zone engine.ZoneView) engine.RouteView {
	r.State, r.Reason, r.Service = engine.RouteFrozen, "account frozen: "+zone.FrozenWhy, ""
	return r
}

// noZone is r without a zone that is served: it has no tunnel and no proof.
func noZone(r engine.RouteView, reason string) engine.RouteView {
	r.State, r.Reason, r.Service, r.Level, r.Zone, r.Account = planner.StateNoZone, reason, "", "", "", ""
	r.Rule, r.Path, r.Candidates = nil, nil, nil
	return r
}

// add adds a route and its guest.
func (m *maker) add(r engine.RouteView) {
	m.f.State.Routes = append(m.f.State.Routes, r)
	if r.Guest == nil {
		return
	}
	m.f.Guests = append(m.f.Guests, engine.GuestListView{
		Ref: r.Owner, Name: r.Guest.Name, Node: m.f.State.Node, Running: true, Tagged: true,
		Identity: "uuid:" + r.Owner, Approval: m.approval(), Routes: 1,
	})
	m.guests++
}

func (m *maker) approval() string {
	if m.f.State.Admission == store.AdmissionApprove {
		return engine.ApprovalApproved
	}
	return engine.ApprovalNotNeeded
}

// fillGuests adds guests that carry no gate tag up to n guests in all.
func (m *maker) fillGuests(n int) {
	for i := 0; len(m.f.Guests) < n; i++ {
		ref := model.GuestRef{Kind: model.KindLXC, VMID: 5000 + i}
		m.f.Guests = append(m.f.Guests, engine.GuestListView{
			Ref: ref.String(), Name: fmt.Sprintf("ct-%04d", i), Node: m.f.State.Node, Running: i%3 != 0,
			Approval: engine.ApprovalNotNeeded,
		})
	}
}

// setRate gives the samples of a tunnel's traffic their figures.
func (m *maker) setRate(tunnel string, rps, errors float64) {
	for i := range m.f.Traffic.Tunnels {
		t := &m.f.Traffic.Tunnels[i]
		if t.TunnelID != tunnel {
			continue
		}
		for j := range t.Samples {
			t.Samples[j].RPS, t.Samples[j].ErrorsPerSec = rps, errors
		}
	}
}

// finish orders what was added, gives every route with a target its figure
// and names the state anew.
func (m *maker) finish() {
	st := &m.f.State
	sortState(st)
	slices.SortStableFunc(m.f.Guests, func(a, b engine.GuestListView) int { return model.CompareOwners(a.Ref, b.Ref) })
	slices.SortStableFunc(m.f.Traffic.Tunnels, func(a, b engine.TunnelTraffic) int { return strings.Compare(a.TunnelID, b.TunnelID) })
	if m.f.Traffic.RoutesWhy == "" {
		m.f.Traffic.Routes = routeTraffic(st.Routes)
		m.f.Traffic.RoutesTotal = len(m.f.Traffic.Routes)
	}
	st.GateTagged = tagged(m.f.Guests)
	st.Digest = engine.DigestOf(*st)
}

// tagged is how many of the guests carry the gate tag.
func tagged(guests []engine.GuestListView) int {
	n := 0
	for _, g := range guests {
		if g.Tagged {
			n++
		}
	}
	return n
}

// routeTraffic gives every route with a target a figure of its own, the same
// for the routes on one target, which share it.
func routeTraffic(routes []engine.RouteView) []engine.RouteTraffic {
	out := []engine.RouteTraffic{}
	on := map[string]int{}
	for _, r := range routes {
		if t := targetOf(r); t != "" {
			on[t]++
		}
	}
	for i, r := range routes {
		t := targetOf(r)
		if t == "" {
			continue
		}
		out = append(out, engine.RouteTraffic{
			Hostname: r.Hostname, Owner: r.Owner, Target: t, FlowsPerSec: float64((i*37)%50) / 10, Shared: on[t] - 1,
		})
	}
	return out
}

// targetOf is the address and port a route is served at, or nothing for one
// that is not served.
func targetOf(r engine.RouteView) string {
	if r.State != planner.StateActive || r.Service == "" {
		return ""
	}
	_, rest, ok := strings.Cut(r.Service, "://")
	if !ok {
		return ""
	}
	if _, err := netip.ParseAddrPort(rest); err != nil {
		return ""
	}
	return rest
}

// hexID is 32 hex digits, as Cloudflare names accounts and zones.
func hexID(kind string, n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s %d", kind, n))
	return hex.EncodeToString(sum[:16])
}
