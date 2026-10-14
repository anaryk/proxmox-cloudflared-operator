package gateway

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// stateCase filters one field of the state for a reader who sees visible,
// with hosts the hostnames of the routes it sees. It never changes what the
// state it is given shares with others: it replaces a list, it does not
// edit it.
type stateCase struct {
	Name  string
	Apply func(st *engine.State, visible auth.Visible, hosts map[string]bool)
}

// stateCases are the cases of FilterState, one per field that is filtered
// or removed (stateFields).
var stateCases = []stateCase{
	{"routes", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		st.Routes = only(st.Routes, func(r engine.RouteView) bool { return ownerVisible(r.Owner, visible) })
		for i := range st.Routes {
			routeFor(&st.Routes[i])
		}
	}},
	{"lease", func(st *engine.State, _ auth.Visible, _ map[string]bool) {
		st.Lease = copied(st.Lease, leaseFields)
	}},
	{"issues", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		st.Issues = only(st.Issues, func(i planner.Issue) bool { return i.Guest == (model.GuestRef{}) || visible(i.Guest) })
	}},
	{"actions", func(st *engine.State, _ auth.Visible, hosts map[string]bool) {
		st.Actions = only(st.Actions, func(a reconcile.Action) bool {
			switch a.Kind {
			case reconcile.CreateTunnel, reconcile.DeleteTunnel, reconcile.PutConfig:
				return true
			}
			return hosts[strings.ToLower(a.Target)]
		})
	}},
	{"conflicts", func(st *engine.State, _ auth.Visible, hosts map[string]bool) {
		st.Conflicts = only(st.Conflicts, func(c reconcile.Conflict) bool { return hosts[strings.ToLower(c.Name)] })
	}},
	{"lost", func(st *engine.State, _ auth.Visible, hosts map[string]bool) {
		st.Lost = only(st.Lost, func(name string) bool { return hosts[strings.ToLower(name)] })
	}},
	{"waiting", func(st *engine.State, _ auth.Visible, _ map[string]bool) { st.Waiting = []engine.Waiting{} }},
	{"offer", func(st *engine.State, _ auth.Visible, _ map[string]bool) { st.Offer = "" }},
	{"unapproved", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		st.Unapproved = only(st.Unapproved, func(u engine.UnapprovedGuest) bool { return visible(u.GuestRef) })
	}},
	{"identity", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		if st.Identity == nil {
			return
		}
		id := *st.Identity
		guests := func(refs []string) []string {
			if refs == nil {
				return nil
			}
			return only(refs, func(ref string) bool { return ownerVisible(ref, visible) && !manual(ref) })
		}
		id.Copies, id.Tenants = guests(id.Copies), guests(id.Tenants)
		st.Identity = &id
	}},
	{"networks", func(st *engine.State, _ auth.Visible, _ map[string]bool) {
		st.Networks = withoutLegMACs(st.Networks)
	}},
	{"managed", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		st.Managed = managedFor(st.Managed, visible)
	}},
	{"nodes", func(st *engine.State, _ auth.Visible, _ map[string]bool) {
		st.Nodes = nodesFor(st.Nodes)
	}},
	{"cluster", func(st *engine.State, visible auth.Visible, _ map[string]bool) {
		st.Cluster = clusterFor(st.Cluster, visible)
	}},
	{"problems", func(st *engine.State, _ auth.Visible, _ map[string]bool) {
		st.Problems = only(st.Problems, func(string) bool { return true })
		for i, line := range st.Problems {
			if managedLine(line) {
				st.Problems[i] = redactMACs(line)
			}
		}
	}},
}

// nodesFor are the nodes as a reader sees them: without the uses of their
// join tokens, which name the sources and containers of joins, and without
// the address of an appliance agent, which is a container's.
func nodesFor(nodes []engine.NodeView) []engine.NodeView {
	out := only(nodes, func(engine.NodeView) bool { return true })
	for i := range out {
		n := &out[i]
		cleared(n, nodeFields)
		if n.Kind != "host" {
			n.AgentAddress = netip.Addr{}
		}
	}
	return out
}

// clusterFor is the cluster as a reader who sees visible sees it: the
// blocks of those guests only, without their MACs and the ports they were
// seen on, which are other guests'.
func clusterFor(c *engine.ClusterView, visible auth.Visible) *engine.ClusterView {
	if c == nil {
		return nil
	}
	v := *c
	cleared(&v, clusterFields)
	if c.Blocks != nil {
		v.Blocks = only(c.Blocks, func(b engine.BlockView) bool { return ownerVisible(b.Guest, visible) && !manual(b.Guest) })
	}
	for i := range v.Blocks {
		cleared(&v.Blocks[i], blockFields)
	}
	cleared(&v.Coverage, coverageFields)
	v.Upgrade = copied(v.Upgrade, upgradeFields)
	v.CARotation = copied(v.CARotation, caRotationFields)
	return &v
}

// routeFor makes a route of the state what a reader gets of it, in place:
// the route is a copy, but its replicas and placement are still the
// state's, and are copied before they change. Their text loses the MACs and
// the ports of guests a node's watch saw.
func routeFor(r *engine.RouteView) {
	cleared(r, routeFields)
	if r.Replicas != nil {
		r.Replicas = only(r.Replicas, func(engine.ReplicaView) bool { return true })
	}
	for i := range r.Replicas {
		p := &r.Replicas[i]
		cleared(p, replicaFields)
		p.Reason = redactMACsAndPorts(p.Reason)
	}
	r.Placement = copied(r.Placement, placementFields)
	if r.Placement != nil {
		r.Placement.Why = redactMACsAndPorts(r.Placement.Why)
		r.Placement.Move = copied(r.Placement.Move, moveFields)
		if r.Placement.Move != nil {
			r.Placement.Move.Step = redactMACsAndPorts(r.Placement.Move.Step)
		}
	}
}

// copied is a copy of what v points to, cleared by decided; nil for nil.
func copied[T any](v *T, decided map[string]decision) *T {
	if v == nil {
		return nil
	}
	c := *v
	cleared(&c, decided)
	return &c
}

// managedLine says that a problem line is one of the managed network's that
// may name a MAC: of a foreign card, of the leg, or of a card in an error.
func managedLine(line string) bool {
	return strings.HasPrefix(line, "foreign card ") || strings.HasPrefix(line, "managed card of ")
}

// withoutLegMACs is a copy of the networks as a reader gets them, as
// networkFields and legFields decide: without the MACs of the legs, which
// an admin sees on the LANs of the bridges, also where the problem of a
// leg names one.
func withoutLegMACs(nets []engine.NetworkView) []engine.NetworkView {
	out := only(nets, func(engine.NetworkView) bool { return true })
	for i := range out {
		cleared(&out[i], networkFields)
		legs := slices.Clone(out[i].Legs)
		for j := range legs {
			legs[j] = legForReader(legs[j])
		}
		out[i].Legs = legs
	}
	return out
}

// legForReader is the leg of a node as a reader gets it.
func legForReader(l engine.LegView) engine.LegView {
	cleared(&l, legFields)
	l.Problem = redactMACs(l.Problem)
	return l
}

// macPattern is a MAC address as the daemon writes one into free text.
var macPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(:[0-9a-f]{2}){5}\b`)

// redactMACs takes every MAC out of free text: the reason of a foreign card
// names the MAC of the leg, and an error of the configurer may name one.
func redactMACs(s string) string { return macPattern.ReplaceAllString(s, "a MAC") }

// portPattern is the name of a port of a guest on a node, as Proxmox names
// them: the tap of a VM, the veth of a container and the links of the
// firewall bridge, with the VMID after the prefix, and a VLAN after a dot.
var portPattern = regexp.MustCompile(`\b(?:tap|veth|fwpr|fwbr|fwln)[0-9][0-9a-z]*(?:\.[0-9]+)?\b`)

// redactMACsAndPorts takes every MAC and every port of a guest out of free
// text: what a node's watch saw names the MAC of one guest and the port of
// another it was seen on.
func redactMACsAndPorts(s string) string {
	return portPattern.ReplaceAllString(redactMACs(s), "a port")
}

// managedFor is the managed network as a reader sees it: the cards, the
// waiting guests and the foreign cards of the guests it may audit, without
// the MACs, the identities, the pveum lines, the firewall of the guests
// before pco changed it and the appliance's isolation, which are an
// admin's. A reason that carries a pveum line or the command of a
// confirmation is cut to its name.
func managedFor(m engine.ManagedView, visible auth.Visible) engine.ManagedView {
	m.Attachments = only(m.Attachments, func(a engine.AttachmentView) bool { return ownerVisible(a.Owner, visible) && !manual(a.Owner) })
	for i := range m.Attachments {
		a := &m.Attachments[i]
		a.MAC, a.Identity, a.GrantCmd, a.Firewall = "", "", "", nil
		a.Detail, a.DetachWhy = redactMACs(a.Detail), redactMACs(a.DetachWhy)
	}
	m.Waiting = only(m.Waiting, func(w engine.ManagedWaiting) bool { return visible(w.GuestRef) })
	for i := range m.Waiting {
		w := &m.Waiting[i]
		w.Identity = ""
		for _, name := range []string{"needs-grant", "needs-firewall-confirmation"} {
			if strings.HasPrefix(w.Why, name+":") {
				w.Why = name
			}
		}
		w.Why = redactMACs(w.Why)
	}
	m.Foreign = only(m.Foreign, func(f engine.ForeignView) bool { return visible(f.GuestRef) })
	for i := range m.Foreign {
		m.Foreign[i].MAC, m.Foreign[i].Why = "", redactMACs(m.Foreign[i].Why)
	}
	m.PoolGrant, m.Filtered = "", nil
	if m.Leg != nil {
		leg := legForReader(*m.Leg)
		m.Leg = &leg
	}
	return m
}

func filterNetworks(raw []byte, _ *Reader) ([]byte, error) {
	return recode(raw, func(nets []engine.NetworkView) (any, error) { return withoutLegMACs(nets), nil })
}

func filterManaged(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(m engine.ManagedView) (any, error) { return managedFor(m, r.Visible), nil })
}

// FilterState is the state a reader who sees visible gets: every case of
// stateCases applied to a copy. The state given is left as it is.
func FilterState(st engine.State, visible auth.Visible) engine.State {
	hosts := visibleHosts(st, visible)
	for _, c := range stateCases {
		c.Apply(&st, visible, hosts)
	}
	return st
}

// visibleHosts are the hostnames of the routes a reader sees, the losing
// ones included, in lower case.
func visibleHosts(st engine.State, visible auth.Visible) map[string]bool {
	hosts := make(map[string]bool)
	for _, r := range st.Routes {
		if ownerVisible(r.Owner, visible) {
			hosts[strings.ToLower(r.Hostname)] = true
		}
	}
	return hosts
}

// only is a new list of the elements of s that keep holds for; never nil.
func only[T any](s []T, keep func(T) bool) []T {
	out := make([]T, 0, len(s))
	for _, v := range s {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func manual(owner string) bool { return strings.HasPrefix(owner, model.ManualPrefix) }

// ownerVisible says whether a reader sees what owner names: a manual route
// is cluster configuration and everyone's; a guest only when visible; a name
// of any other form nobody's.
func ownerVisible(owner string, visible auth.Visible) bool {
	if manual(owner) {
		return true
	}
	ref, err := model.ParseGuestRef(owner)
	return err == nil && visible(ref)
}

// keepEvent says whether a reader gets an event: by its guest, or, without
// one, by its route, or by a subject that names a guest, as the approvals
// do. Its free text may still name a guest the reader does not see: a claim
// conflict quotes both owners.
func keepEvent(ev engine.Event, visible auth.Visible, hosts map[string]bool) bool {
	switch {
	case ev.Guest != "":
		return ownerVisible(ev.Guest, visible) && !manual(ev.Guest)
	case ev.Route != "":
		return hosts[strings.ToLower(ev.Route)]
	}
	if ref, err := model.ParseGuestRef(ev.Subject); err == nil {
		return visible(ref)
	}
	return true
}

// adminKinds are the kinds of event no reader gets whatever they are about:
// enrolment names the sources and the containers of joins, and an
// attestation, a placement or a block what a node's watch saw of other
// guests. The replicas and the placement of a route say what a reader needs
// of the last three.
var adminKinds = map[string]bool{"enrol": true, "attestation": true, "placement": true, "block": true}

// readerKeeps says whether a reader gets an event: never one of the audit
// trail, whatever its kind, nor one of adminKinds, nor one an agent sent
// under such a kind, which the controller keeps as agent:<kind>; the others
// by what they are about.
func readerKeeps(ev engine.Event, visible auth.Visible, hosts map[string]bool) bool {
	return !ev.Security && !adminKinds[ev.Kind] && !strings.HasPrefix(ev.Kind, "agent:") && keepEvent(ev, visible, hosts)
}

// readerEvent is what a reader gets of an event, and whether it gets it: a
// copy without the admin who acted and the account, with the MACs and the
// ports of guests that the text of an event of the managed network names
// taken out.
func readerEvent(ev engine.Event, visible auth.Visible, hosts map[string]bool) (engine.Event, bool) {
	if !readerKeeps(ev, visible, hosts) {
		return engine.Event{}, false
	}
	ev.Actor, ev.Account = "", ""
	if ev.Kind == "managed" {
		ev.Message = redactMACsAndPorts(ev.Message)
	}
	return ev, true
}

// FilterEvents are the events a reader gets of evs, each as readerEvent
// makes it, a new list.
func FilterEvents(evs []engine.Event, visible auth.Visible, hosts map[string]bool) []engine.Event {
	out := make([]engine.Event, 0, len(evs))
	for _, ev := range evs {
		if ev, ok := readerEvent(ev, visible, hosts); ok {
			out = append(out, ev)
		}
	}
	return out
}

// FilterTraffic keeps the routes whose owner the reader sees, counts in
// Shared only the others of them on the same target, and sets RoutesTotal
// to how many of them have a figure, so that a reader cannot count the
// routes of guests it does not see. The tunnels and why there are no
// figures stay.
func FilterTraffic(tv engine.TrafficView, visible auth.Visible) engine.TrafficView {
	tv.Routes = visibleFigures(tv.Routes, visible)
	tv.RoutesTotal = len(tv.Routes)
	return tv
}

// visibleFigures are the figures of the routes a reader sees, with Shared
// counted among them.
func visibleFigures(routes []engine.RouteTraffic, visible auth.Visible) []engine.RouteTraffic {
	out := only(routes, func(r engine.RouteTraffic) bool { return ownerVisible(r.Owner, visible) })
	shared := make(map[string]int)
	for _, r := range out {
		shared[r.Target]++
	}
	for i := range out {
		out[i].Shared = shared[out[i].Target] - 1
	}
	return out
}

// DoctorCounts is what a reader gets of the doctor: how many findings of
// each level, run at at.
func DoctorCounts(fs []doctor.Finding, at time.Time) wire.DoctorCounts {
	c := wire.DoctorCounts{At: at.UTC()}
	for _, f := range fs {
		switch f.Level {
		case doctor.LevelOK:
			c.OK++
		case doctor.LevelWarn:
			c.Warn++
		case doctor.LevelFail:
			c.Fail++
		}
	}
	return c
}

// recode decodes raw into a T, filters it and encodes it again.
func recode[T any](raw []byte, fn func(T) (any, error)) ([]byte, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("reading the answer of the daemon: %w", err)
	}
	out, err := fn(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func filterEvents(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(evs []engine.Event) (any, error) {
		_, view, err := r.state()
		if err != nil {
			return nil, err
		}
		return FilterEvents(evs, r.Visible, view.hosts), nil
	})
}

func filterTraffic(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(tv engine.TrafficView) (any, error) { return FilterTraffic(tv, r.Visible), nil })
}

// filterRouteSeries counts in Shared only the other routes on the same
// target that the reader sees.
func filterRouteSeries(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(s engine.RouteSeries) (any, error) {
		s.Shared = 0
		for _, f := range r.figures() {
			if f.Target == s.Target && f.Hostname != s.Hostname {
				s.Shared++
			}
		}
		return s, nil
	})
}

// figures are the figures of the routes the reader sees; none when the
// daemon did not give them, and then nothing is counted.
func (r *Reader) figures() []engine.RouteTraffic {
	data, err := r.g.get(r.ctx, r.actor, "/v1/traffic", nil)
	var tv engine.TrafficView
	if err == nil {
		err = json.Unmarshal(data, &tv)
	}
	if err != nil {
		r.g.log.Warn().Err(err).Msg("reading the figures of the routes for a reader")
		return nil
	}
	return visibleFigures(tv.Routes, r.Visible)
}

func filterClaims(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(claims []engine.ClaimView) (any, error) {
		out := only(claims, func(c engine.ClaimView) bool { return ownerVisible(c.Holder, r.Visible) })
		for i := range out {
			out[i].Waiting = only(out[i].Waiting, func(w engine.ClaimantView) bool { return ownerVisible(w.Owner, r.Visible) })
		}
		return out, nil
	})
}

func filterApprovals(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(as []engine.ApprovalView) (any, error) {
		return only(as, func(a engine.ApprovalView) bool { return ownerVisible(a.Owner, r.Visible) && !manual(a.Owner) }), nil
	})
}

func filterGuests(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(gs []engine.GuestListView) (any, error) {
		return only(gs, func(g engine.GuestListView) bool { return ownerVisible(g.Ref, r.Visible) && !manual(g.Ref) }), nil
	})
}

func filterDoctor(raw []byte, r *Reader) ([]byte, error) {
	return recode(raw, func(fs []doctor.Finding) (any, error) { return DoctorCounts(fs, r.g.now()), nil })
}

// checkAnnotation refuses the annotation of a guest the reader does not
// see. A path that names no guest goes up, and the daemon refuses it.
func checkAnnotation(r *Reader, c *gin.Context, _ url.Values) error {
	ref, err := model.ParseGuestRef(c.Param("kind") + "/" + c.Param("vmid"))
	if err != nil || r.Visible(ref) {
		return nil
	}
	return errHidden
}

// checkHolder lets a reader read the figures of the target of a hostname
// only when the route that holds it is the reader's to see: that of a
// visible guest, or a manual route. The holder is the one the daemon
// diagnoses, chosen by its own function, so a reader who sees only the
// losing route of a hostname reads nothing of the holder's. A hostname
// without a route and one of another's answer alike.
func checkHolder(r *Reader, _ *gin.Context, q url.Values) error {
	_, err := visibleHolder(r, q)
	return err
}

// checkDiagnosis lets a reader diagnose a hostname as checkHolder lets it
// read its figures, and names the holder it checked to the daemon, which
// refuses when another holds the hostname by the time it walks the chain.
func checkDiagnosis(r *Reader, _ *gin.Context, q url.Values) error {
	owner, err := visibleHolder(r, q)
	if err == nil {
		q.Set("owner", owner)
	}
	return err
}

// visibleHolder is the owner of the route that holds the hostname of q, when
// the reader sees it.
func visibleHolder(r *Reader, q url.Values) (string, error) {
	names := q["hostname"]
	if len(names) != 1 {
		return "", errHidden
	}
	host, err := hostname.Normalize(names[0])
	if err != nil {
		return "", errHidden
	}
	st, _, err := r.state()
	if err != nil {
		return "", err
	}
	holder, ok := doctor.HolderOf(st, host)
	if !ok || !ownerVisible(holder.Owner, r.Visible) {
		return "", errHidden
	}
	return holder.Owner, nil
}

// trafficNotice is a traffic notice for a reader: the routes it sees, with
// Shared and RoutesTotal counted among the routes it sees of all, the
// figures of every route; without them among those of the notice.
func trafficNotice(n engine.TrafficNotice, visible auth.Visible, all []engine.RouteTraffic) engine.TrafficNotice {
	n.Routes = only(n.Routes, func(r engine.RouteTraffic) bool { return ownerVisible(r.Owner, visible) })
	if all == nil {
		all = n.Routes
	}
	figures := visibleFigures(all, visible)
	shared := make(map[string]int, len(figures))
	for _, f := range figures {
		shared[f.Target]++
	}
	for i := range n.Routes {
		n.Routes[i].Shared = max(0, shared[n.Routes[i].Target]-1)
	}
	if n.RoutesWhy == "" {
		n.RoutesTotal = len(figures)
	}
	return n
}

// unseenGap stands for events from to to of the boot that the web does not
// know of. Their levels are not known either, and the daemon's would say what
// guests the reader may not see did: it says warn, so that the page looks.
func unseenGap(boot string, from, to uint64) engine.GapNotice {
	return engine.GapNotice{Boot: boot, From: from, To: to, Count: int(to - from + 1), Level: "warn"}
}

// readerGap is a gap of the daemon as a reader gets it: of the events in it,
// those the reader sees, counted with their highest level, over the same
// seqs; none when the reader sees none of them. When the events were not
// read, the reader is told of the whole range, as of events unknown: one
// that could be told less would miss what it may see.
func readerGap(g engine.GapNotice, in []engine.Event, known bool, visible auth.Visible, hosts map[string]bool) (engine.GapNotice, bool) {
	if !known {
		return unseenGap(g.Boot, g.From, g.To), true
	}
	var out *engine.GapNotice
	for _, ev := range in {
		if ev.Seq >= g.From && ev.Seq <= g.To && readerKeeps(ev, visible, hosts) {
			out = joined(out, gapOf(ev))
		}
	}
	if out == nil {
		return engine.GapNotice{}, false
	}
	out.Boot, out.From, out.To = g.Boot, g.From, g.To
	return *out, true
}
