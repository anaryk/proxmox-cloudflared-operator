package engine

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ManagedView is the managed network as a cycle found it.
type ManagedView struct {
	Enabled     bool   `json:"enabled"`     // the network pconet is stored
	Consent     string `json:"consent"`     // per-guest or auto
	AutoAllowed bool   `json:"autoAllowed"` // the gate tag is registered admin-only
	// GateTag is, under consent auto, the tag that consents for every guest
	// it admits, whether the guest needs the network yet or not; empty under
	// per-guest consent.
	GateTag string `json:"gateTag,omitempty"`
	// AutoStopped says why automatic attaches are stopped; empty while they
	// run, and under per-guest consent.
	AutoStopped string           `json:"autoStopped,omitempty"`
	TagHonoured bool             `json:"tagHonoured"` // cf-tunnel-managed is registered
	Subnet      netip.Prefix     `json:"subnet,omitzero"`
	Leg         *LegView         `json:"leg,omitempty"`
	Attachments []AttachmentView `json:"attachments"`
	// Waiting are the guests that would be attached, each with why they are
	// not; Foreign the cards on pconet that no attachment explains.
	Waiting []ManagedWaiting `json:"waiting"`
	Foreign []ForeignView    `json:"foreign"`
	Free    int              `json:"free"` // addresses left
	Vnet    VnetView         `json:"vnet"` // what the cycle read of pconet
	// PoolGrant is, in the appliance, the pool the cards of whose guests pco
	// may change.
	PoolGrant string `json:"poolGrant,omitempty"`
	// Refused says why the managed network cannot run on this node now: the
	// vnet is not as pco made it, or the appliance's node cannot isolate
	// the guests. Nothing attaches while it stands; attachments are kept.
	Refused string `json:"refused,omitempty"`
	// Filtered is the appliance's isolation of its guests; nil on the host
	// and until it is judged. DHCP is the server for the managed guests;
	// nil until it runs.
	Filtered *FilteredView `json:"filtered,omitempty"`
	DHCP     *DHCPView     `json:"dhcp,omitempty"`
}

// The states of AttachmentView.State.
const (
	AttachmentAttached      = "attached"
	AttachmentUnverified    = "attached (unverified)" // the card could not be added or checked this cycle
	AttachmentPendingReboot = "pending-reboot"
	AttachmentAwaitingDHCP  = "awaiting-dhcp"
	AttachmentNeedsGrant    = "needs-grant"
	AttachmentDetaching     = "detaching"
	AttachmentDetachPending = "detach-pending"
	AttachmentMigrated      = "migrated"
)

// AttachmentView is a card pco put on a guest, with how it stands.
type AttachmentView struct {
	store.Attachment
	Guest *GuestView `json:"guest,omitempty"`
	// State is one of the Attachment constants: attached, attached
	// (unverified), pending-reboot, awaiting-dhcp, needs-grant, detaching,
	// detach-pending or migrated. Detail says more where there is more to
	// say, and GrantCmd is the pveum line of needs-grant.
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	GrantCmd string `json:"grantCmd,omitempty"`
}

// ManagedWaiting is a guest that would get a card, and why it does not.
type ManagedWaiting struct {
	GuestView
	Identity string `json:"identity"`
	Why      string `json:"why"`
}

// The actions of ForeignView.Action.
const (
	ForeignReplaced = "replaced"
	ForeignLinkDown = "link-down"
	ForeignRemoved  = "removed"
	ForeignReported = "reported"
)

// ForeignView is a card on the managed network that no attachment explains,
// and what pco did about it.
type ForeignView struct {
	GuestView
	NIC    int    `json:"nic"`
	MAC    string `json:"mac"`
	Action string `json:"action"` // replaced, link-down, removed, reported
	Why    string `json:"why"`
}

// VnetView is what the cycle read of the vnet pconet; any field off the
// value pco made it with holds the attaches.
type VnetView struct {
	OK           bool     `json:"ok"`
	ZoneType     string   `json:"zoneType"`
	IsolatePorts bool     `json:"isolatePorts"` // on on the host, off in the appliance
	Subnets      int      `json:"subnets"`      // 0
	Why          []string `json:"why,omitempty"`
}

// FilteredView says whether the appliance's guests are isolated by their
// firewall: the predicate of the filtered level.
type FilteredView struct {
	Holds   bool     `json:"holds"`
	Backend string   `json:"backend"` // pve-firewall or proxmox-firewall
	Offered bool     `json:"offered"` // false on a backend filtered is not offered on
	Why     []string `json:"why,omitempty"`
}

// DHCPView is the state of the DHCP server for the managed guests.
type DHCPView struct {
	Bound   bool      `json:"bound"`
	Since   time.Time `json:"since,omitzero"`
	Unknown int       `json:"unknown"` // requests from MACs without a lease, since start
}

// ManagedVnet is what Deps.Vnet reads of the vnet pconet.
type ManagedVnet struct {
	Exists       bool
	ZoneType     string
	IsolatePorts bool
	Subnets      int
}

const (
	zoneTypeSimple = "simple"
	firewallUnit   = "pve-firewall"
	// unknownMACEvery is how often the same unknown MAC is reported.
	unknownMACEvery = 5 * time.Minute
	// maxUnknownMACs is how many unknown MACs have an event within that time.
	maxUnknownMACs = 64
	// vnetEvery is how long what was read of the vnet stands, and
	// impactEvery how long what was read of a guest's firewall does while
	// its configuration is the same.
	vnetEvery   = time.Minute
	impactEvery = time.Minute
	// writeWaits starts the reason of what waits while the cycle may not
	// write.
	writeWaits = "waits until the cycle may write: "
)

// vnetRead is the vnet as the step last read it, and when.
type vnetRead struct {
	at   time.Time
	view VnetView
}

// impactRead is what the step last read of the firewall of a guest: the
// card turning it on would filter, or why it could not be read, for the
// configuration of the digest.
type impactRead struct {
	at     time.Time
	digest string
	card   string
	why    string
}

// The problem lines of the managed network.
const (
	problemVnetChanged    = "the vnet pconet was changed outside pco: %s"
	problemManagedRefused = "the managed network cannot run on this node: %s"
	problemAutoStopped    = "managed.consent is auto but the gate tag %s is not registered as admin-only; automatic attaches are stopped; attached guests stay attached"
	problemAutoUnreadable = "managed.consent is auto, and registered-tags could not be read; automatic attaches are stopped; attached guests stay attached"
	problemForeignCard    = "foreign card net%d (%s) of %s: %s"
	problemManagedCard    = "managed card of %s: %s"
	problemManagedStore   = "the managed network is left as it is: %s: %v"

	autoUnreadable = "registered-tags could not be read"
)

func (v ManagedView) clone() ManagedView {
	v.Attachments = slices.Clone(v.Attachments)
	for i := range v.Attachments {
		a := &v.Attachments[i]
		if a.Guest != nil {
			g := *a.Guest
			a.Guest = &g
		}
		a.Firewall = cloneP(a.Firewall)
		a.DetachAt, a.RemovedAt = cloneP(a.DetachAt), cloneP(a.RemovedAt)
	}
	v.Waiting = slices.Clone(v.Waiting)
	v.Foreign = slices.Clone(v.Foreign)
	v.Vnet.Why = slices.Clone(v.Vnet.Why)
	v.Leg = cloneP(v.Leg)
	v.DHCP = cloneP(v.DHCP)
	if v.Filtered != nil {
		f := *v.Filtered
		f.Why = slices.Clone(f.Why)
		v.Filtered = &f
	}
	return v
}

func cloneP[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func (v ManagedView) normalized() ManagedView {
	v.Attachments = nonNil(v.Attachments)
	v.Waiting = nonNil(v.Waiting)
	v.Foreign = nonNil(v.Foreign)
	slices.SortFunc(v.Attachments, func(a, b AttachmentView) int { return model.CompareOwners(a.Owner, b.Owner) })
	slices.SortFunc(v.Waiting, func(a, b ManagedWaiting) int { return model.CompareOwners(a.String(), b.String()) })
	slices.SortFunc(v.Foreign, func(a, b ForeignView) int {
		return cmp.Or(model.CompareOwners(a.String(), b.String()), a.NIC-b.NIC)
	})
	return v
}

// managedStep is the step of the managed network of one cycle.
type managedStep struct {
	c    *cycleRun
	prev ManagedView
	v    ManagedView
	net  store.Network
	ipam *managednet.IPAM

	registered  []string
	consents    map[string]store.Consent
	attachments map[string]store.Attachment
	leases      map[string]store.Lease
	writable    map[string]bool
	grant       map[string]string
	// hold says why nothing attaches this cycle; empty when attaches run.
	hold string
	// writeHold says why the cycle may not write, which leaves every card
	// as it is but for the link of a copy; empty when it may. heldDetach are
	// the owners whose detach waits for it.
	writeHold  string
	heldDetach map[string]bool
	// unverified says, by owner, why the card of an attachment could not
	// be added or checked this cycle.
	unverified map[string]string
	// migrated are the owners whose attachment is on another node.
	migrated map[string]bool
}

// managed puts consenting guests on the managed network, and takes the
// cards of those whose consent ended after the grace. It runs after the
// routes are collected and before the addresses are resolved, so that a
// card added live is a candidate of the same cycle, and never on an
// incomplete snapshot.
func (c *cycleRun) managed() {
	s := &managedStep{c: c, prev: c.st.Managed, unverified: map[string]string{}, migrated: map[string]bool{}, heldDetach: map[string]bool{}}
	c.st.Managed = s.run()
	s.dhcp()
}

func (s *managedStep) run() ManagedView {
	c := s.c
	settings := c.settings.Managed
	s.v = ManagedView{Consent: settings.Consent, Subnet: settings.Subnet, Attachments: []AttachmentView{}, Waiting: []ManagedWaiting{}, Foreign: []ForeignView{}}
	if settings.Consent == store.ConsentAuto {
		s.v.GateTag = c.settings.GateTag
	}
	nets, err := c.e.d.Store.Networks()
	if err != nil {
		c.problem(problemManagedStore, "reading the networks", err)
		return s.prev
	}
	n, enabled := nets[gateway.ManagedBridge]
	s.v.Enabled = enabled
	if !enabled {
		return s.v
	}
	s.net = n
	s.v.Subnet = n.Prefix
	s.v.Leg = s.legView()
	s.readRegistered()
	s.v.Vnet = s.checkVnet()
	if c.appliance() {
		s.v.Refused = s.checkIsolation()
		s.v.PoolGrant = s.poolGrant()
	}
	if c.e.d.Guests == nil && s.v.Refused == "" {
		s.v.Refused = "this daemon cannot change the cards of guests"
	}
	switch {
	case s.v.Refused != "":
		s.hold = fmt.Sprintf(problemManagedRefused, s.v.Refused)
	case !s.v.Vnet.OK:
		s.hold = fmt.Sprintf(problemVnetChanged, strings.Join(s.v.Vnet.Why, "; "))
	}
	if s.hold != "" {
		c.problem("%s", s.hold)
	}
	if !c.writerRead || c.cfHold || c.storeHold {
		s.writeHold = writeWaits + cmp.Or(c.holdWhy, gateNoWriter)
	}
	if !s.read() {
		return s.v
	}
	s.ipam = managednet.NewIPAM(c.e.d.Store, n.Prefix, func() time.Time { return c.now })
	in := s.input()
	extra := s.impacts(&in)
	decisions := managednet.Decide(in)
	s.v.Waiting = append(s.v.Waiting, extra...)
	for _, d := range decisions {
		s.foreign(d)
	}
	for _, d := range decisions {
		if d.Action == managednet.ActionDetach {
			s.detach(d)
		}
	}
	for _, d := range decisions {
		switch d.Action {
		case managednet.ActionAttach:
			s.attach(d)
		case managednet.ActionKeep:
			s.keep(d)
		case managednet.ActionWait:
			if !s.migratedNow(d.Owner) {
				s.wait(d.Owner, d.Why)
			}
		}
	}
	s.firewalls()
	s.noteAuto()
	s.fill()
	s.events()
	return s.v
}

// read reads the consents, the attachments and the leases; without them the
// step decides nothing.
func (s *managedStep) read() bool {
	c := s.c
	var err error
	if s.consents, err = c.e.d.Store.Consents(); err != nil {
		c.problem(problemManagedStore, "reading the consents", err)
		return false
	}
	if s.attachments, err = c.e.d.Store.Attachments(); err != nil {
		c.problem(problemManagedStore, "reading the attachments", err)
		return false
	}
	if s.leases, err = c.e.d.Store.Leases(); err != nil {
		c.problem(problemManagedStore, "reading the leases", err)
		return false
	}
	return true
}

// readRegistered reads the registered tags of the datacenter once per
// cycle; nil when they could not be read.
func (s *managedStep) readRegistered() {
	c := s.c
	if c.e.d.RegisteredTags == nil {
		return
	}
	tags, err := c.e.d.RegisteredTags(c.ctx)
	if err != nil {
		c.e.d.Log.Debug().Err(err).Msg("reading the registered tags failed")
		return
	}
	if tags == nil {
		tags = []string{}
	}
	s.registered = tags
	s.v.TagHonoured = slices.Contains(tags, managednet.Tag)
	s.v.AutoAllowed = slices.Contains(tags, c.settings.GateTag)
}

// checkVnet reads the vnet and says what differs from how pco made it: a
// zone of type simple, isolated ports on a host and not in the appliance,
// and no subnet, so that pco's leg is the only gateway.
func (s *managedStep) checkVnet() VnetView {
	c := s.c
	if c.e.d.Vnet == nil {
		return VnetView{OK: true, ZoneType: zoneTypeSimple, IsolatePorts: !c.appliance()}
	}
	if last := c.e.vnetRead; !last.at.IsZero() && c.now.Sub(last.at) >= 0 && c.now.Sub(last.at) < vnetEvery {
		return last.view
	}
	view := s.readVnet()
	c.e.vnetRead = vnetRead{at: c.now, view: view}
	return view
}

// readVnet reads the vnet and says what differs from how pco made it.
func (s *managedStep) readVnet() VnetView {
	c := s.c
	vnet, err := c.e.d.Vnet(c.ctx)
	if err != nil {
		return VnetView{Why: []string{"it could not be read: " + err.Error()}}
	}
	view := VnetView{ZoneType: vnet.ZoneType, IsolatePorts: vnet.IsolatePorts, Subnets: vnet.Subnets, Why: vnetWhy(vnet, c.appliance())}
	view.OK = len(view.Why) == 0
	return view
}

// vnetWhy says what differs between the vnet as read and as pco makes it
// for the profile.
func vnetWhy(v ManagedVnet, appliance bool) []string {
	if !v.Exists {
		return []string{"it does not exist"}
	}
	var why []string
	if v.ZoneType != zoneTypeSimple {
		why = append(why, fmt.Sprintf("its zone is of type %s, not %s", cmp.Or(v.ZoneType, "none"), zoneTypeSimple))
	}
	switch {
	case appliance && v.IsolatePorts:
		why = append(why, "isolate-ports is on, and in the appliance it is off")
	case !appliance && !v.IsolatePorts:
		why = append(why, "isolate-ports is off, and on a host it is on")
	}
	if v.Subnets > 0 {
		why = append(why, fmt.Sprintf("it has %s, and pco's leg is its only gateway", plural(v.Subnets, "subnet", "subnets")))
	}
	return why
}

// checkIsolation says why the appliance's node cannot isolate the guests
// of the managed network: the nftables backend installs no guest rules,
// and without the datacenter firewall or pve-firewall running no guest rule
// is in force.
func (s *managedStep) checkIsolation() string {
	c := s.c
	node := c.e.d.Node
	if c.e.d.NodeFirewall == nil || c.e.d.ClusterFirewall == nil || c.e.d.NodeService == nil {
		return "this daemon cannot read the firewall of the node"
	}
	opts, err := c.e.d.NodeFirewall(c.ctx)
	b := s.noteBackend()
	b.node, b.nodeErr = opts, err
	switch {
	case err != nil:
		return fmt.Sprintf("the firewall options of node %s could not be read: %v", node, err)
	case opts.NFTables:
		return fmt.Sprintf("the firewall of node %s runs on the nftables backend (nftables: 1), which installs no guest rules; "+
			"set nftables: 0 in its firewall options", node)
	}
	dc, err := c.e.d.ClusterFirewall(c.ctx)
	b.cluster, b.clusterErr = dc, err
	switch {
	case err != nil:
		return fmt.Sprintf("the firewall options of the datacenter could not be read: %v", err)
	case !dc.Enabled:
		return "the datacenter firewall is off; no guest rule is in force until it is turned on"
	}
	state, err := c.e.d.NodeService(c.ctx, firewallUnit)
	b.service, b.serviceErr = state, err
	switch {
	case err != nil:
		return fmt.Sprintf("the state of %s on node %s could not be read: %v", firewallUnit, node, err)
	case state != "running":
		return fmt.Sprintf("%s is %s on node %s; no guest rule is in force until it runs", firewallUnit, state, node)
	}
	return ""
}

// poolGrant is the pool whose guests' cards pco's user or token may change,
// as the access control last read grants it.
func (s *managedStep) poolGrant() string {
	own := s.c.e.d.OwnUser
	for _, e := range s.c.e.accessNow(s.c.now, s.c.snap).data.ACL {
		if e.RoleID != managedRole || !strings.HasPrefix(e.Path, "/pool/") {
			continue
		}
		if e.UGID == own || strings.HasPrefix(e.UGID, own+"!") {
			return strings.TrimPrefix(e.Path, "/pool/")
		}
	}
	return ""
}

// managedRole is the role the appliance's installer grants on the guests
// whose cards pco may change.
const managedRole = "PCOManaged"

// legView is the leg of the managed network on this node, as the gateway
// last said.
func (s *managedStep) legView() *LegView {
	addr, ok := s.net.Legs[s.c.e.d.Node]
	if !ok {
		return nil
	}
	views := networkViews(map[string]store.Network{s.net.ID: s.net}, nil, nil, s.c.e.LegStates(), s.c.e.d.Node)
	if len(views) == 0 || len(views[0].Legs) == 0 {
		return &LegView{Node: s.c.e.d.Node, Addr: addr}
	}
	leg := views[0].Legs[0]
	return &leg
}

// input is what Decide reads, from the snapshot and what the cycle read
// with it, without the firewall impact, which impacts adds for the guests
// it matters for.
func (s *managedStep) input() managednet.DecideInput {
	c := s.c
	appliance := c.appliance()
	in := managednet.DecideInput{
		Now: c.now, Settings: c.settings.Managed, GateTag: c.settings.GateTag, Appliance: appliance, Registered: s.registered,
		OptOut: c.col.OptOut, Needs: map[string]bool{}, ValidRoutes: map[string]int{}, AdmissionMode: c.settings.Admission,
		Admitted: map[string]bool{}, Consents: s.consents, Attachments: s.attachments, Leases: s.leases, Node: c.e.d.Node,
		Writable: map[string]bool{}, Grant: map[string]string{}, Impact: map[string]string{}, AcceptImpact: map[string]bool{},
	}
	var self model.GuestRef
	if appliance && c.install.Appliance != nil {
		self = model.GuestRef{Kind: model.KindLXC, VMID: c.install.Appliance.VMID}
	}
	in.Foreign = managednet.Classify(c.snap.Guests, s.attachments, self, s.net.LegMAC)
	for _, rt := range c.col.Routes {
		in.ValidRoutes[rt.Owner()]++
	}
	reached := s.reaches(self)
	for _, g := range c.snap.Guests {
		if g.Template || !g.HasTag(c.settings.GateTag) {
			continue
		}
		owner := g.Ref.String()
		in.Needs[owner] = needs(g, reached, c.col.Routes)
		if a, ok := c.approvals[owner]; ok && g.Identity != "" && a.Identity == g.Identity {
			in.Admitted[owner] = true
		}
		if consent, ok := s.consents[owner]; ok && consent.Identity == g.Identity {
			in.AcceptImpact[owner] = consent.AcceptFirewallImpact
		}
		in.Guests = append(in.Guests, g)
	}
	owners := map[string]bool{}
	for _, g := range in.Guests {
		owners[g.Ref.String()] = true
	}
	for _, f := range in.Foreign {
		owners[f.Owner] = true
	}
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		ref, _ := model.ParseGuestRef(owner)
		g, ok := c.snap.Guest(ref)
		if !ok {
			continue
		}
		ok, why := s.canWrite(g)
		in.Writable[owner] = ok
		if why != "" {
			in.Grant[owner] = why
		}
	}
	s.writable, s.grant = in.Writable, in.Grant
	return in
}

// impacts adds to in the firewall impact of the guests it matters for in
// the appliance: those Decide would give a card now, attached or put in the
// place of a copy. The firewall of no other guest is read. A guest whose
// firewall could not be read is taken out of in and waits with the reason.
func (s *managedStep) impacts(in *managednet.DecideInput) []ManagedWaiting {
	c := s.c
	if !in.Appliance {
		return nil
	}
	ask := map[string]bool{}
	for _, d := range managednet.Decide(*in) {
		_, attached := in.Attachments[d.Owner]
		if d.Action == managednet.ActionReplace || d.Action == managednet.ActionAttach && !attached {
			ask[d.Owner] = true
		}
	}
	var extra []ManagedWaiting
	in.Guests = slices.DeleteFunc(in.Guests, func(g model.Guest) bool {
		owner := g.Ref.String()
		if !ask[owner] {
			return false
		}
		card, why := s.impact(g)
		if why != "" {
			extra = append(extra, ManagedWaiting{GuestView: *c.guestView(g.Ref), Identity: g.Identity, Why: why})
			return true
		}
		if card != "" {
			in.Impact[owner] = card
		}
		return false
	})
	return extra
}

// canWrite asks the configurer whether pco may change the guest, and why
// not when it may not.
func (s *managedStep) canWrite(g model.Guest) (bool, string) {
	if s.c.e.d.Guests == nil {
		return false, ""
	}
	ok, why, err := s.c.e.d.Guests.CanWrite(s.c.ctx, g)
	if err != nil {
		return false, "whether pco may change the guest could not be read: " + err.Error()
	}
	return ok, why
}

// impact names the card of a guest that turning its firewall on would newly
// filter: another card with firewall=1 while the guest firewall is off.
// why says that the guest firewall could not be read.
func (s *managedStep) impact(g model.Guest) (card, why string) {
	i := slices.IndexFunc(g.NICs, func(n model.NIC) bool { return n.Bridge != gateway.ManagedBridge && n.Firewall })
	if i < 0 {
		return "", ""
	}
	owner, now := g.Ref.String(), s.c.now
	if last, ok := s.c.e.impacts[owner]; ok && last.digest == g.Digest && now.Sub(last.at) >= 0 && now.Sub(last.at) < impactEvery {
		return last.card, last.why
	}
	card, why = s.readImpact(g, i)
	s.c.e.impacts[owner] = impactRead{at: now, digest: g.Digest, card: card, why: why}
	return card, why
}

// readImpact reads the firewall of the guest whose card i has firewall=1.
func (s *managedStep) readImpact(g model.Guest, i int) (card, why string) {
	if s.c.e.d.GuestFirewall == nil {
		return "", fmt.Sprintf("the firewall options of %s cannot be read by this daemon", g.Ref)
	}
	opts, err := s.c.e.d.GuestFirewall(s.c.ctx, g)
	switch {
	case err != nil:
		return "", fmt.Sprintf("the firewall options of %s could not be read: %v", g.Ref, err)
	case opts.Enabled:
		return "", ""
	}
	return "net" + strconv.Itoa(g.NICs[i].Index), ""
}

// reaches are the segments pco reaches without the managed network: on the
// host those the node has an address in, in the appliance those of its own
// cards, and the attached networks with a leg on this node.
func (s *managedStep) reaches(self model.GuestRef) map[resolve.Segment]bool {
	c := s.c
	out := map[resolve.Segment]bool{}
	if self != (model.GuestRef{}) {
		if g, ok := c.snap.Guest(self); ok {
			for _, nic := range g.NICs {
				if nic.Bridge != gateway.ManagedBridge {
					out[resolve.SegmentOf(nic)] = true
				}
			}
		}
	} else {
		for _, n := range c.snap.Nodes {
			if n.Name != c.e.d.Node {
				continue
			}
			for _, ifc := range n.Ifaces {
				if seg, ok := ifaceSegment(ifc); ok {
					out[seg] = true
				}
			}
		}
	}
	nets, err := c.e.d.Store.Networks()
	if err != nil {
		return out
	}
	for _, n := range nets {
		if _, ok := n.Legs[c.e.d.Node]; ok && n.Kind == store.NetworkAttach {
			out[resolve.Segment{Bridge: n.Bridge, VLAN: n.VLAN}] = true
		}
	}
	return out
}

// ifaceSegment is the segment of guests an interface of the node with an
// address reaches: a bridge, or a VLAN of one (vmbr2.30).
func ifaceSegment(ifc pve.NodeIface) (resolve.Segment, bool) {
	if len(ifc.Addrs) == 0 {
		return resolve.Segment{}, false
	}
	if ifc.Type == "bridge" {
		return resolve.Segment{Bridge: ifc.Name}, true
	}
	bridge, tag, ok := strings.Cut(ifc.Name, ".")
	if !ok || !strings.HasPrefix(bridge, "vmbr") {
		return resolve.Segment{}, false
	}
	vlan, err := strconv.Atoi(tag)
	if err != nil || vlan < 1 {
		return resolve.Segment{}, false
	}
	return resolve.Segment{Bridge: bridge, VLAN: vlan}, true
}

// needs says whether a guest needs the managed network: none of its cards
// off it is on a segment pco reaches, or a route of its says via=managed.
func needs(g model.Guest, reached map[resolve.Segment]bool, routes []model.Route) bool {
	owner := g.Ref.String()
	for _, rt := range routes {
		if rt.Owner() == owner && rt.Options.Via == ViaManaged {
			return true
		}
	}
	for _, nic := range g.NICs {
		if nic.Bridge != gateway.ManagedBridge && reached[resolve.SegmentOf(nic)] {
			return false
		}
	}
	return true
}

// migratedNow says that owner has an attachment and is on another node now,
// which its attachment shows: it does not wait as well.
func (s *managedStep) migratedNow(owner string) bool {
	if _, attached := s.attachments[owner]; !attached {
		return false
	}
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return false
	}
	g, ok := s.c.snap.Guest(ref)
	return ok && g.Node != s.c.e.d.Node
}

// wait lists a guest that would be attached, with why it is not.
func (s *managedStep) wait(owner, why string) {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return
	}
	w := ManagedWaiting{GuestView: *s.c.guestView(ref), Why: why}
	if g, ok := s.c.snap.Guest(ref); ok {
		w.Identity = g.Identity
	}
	s.v.Waiting = append(s.v.Waiting, w)
}

// spec is the card of a lease for a guest: its MAC, for a container its
// address, and in the appliance the firewall on.
func (s *managedStep) spec(g model.Guest, mac string, addr netip.Addr) managednet.NICSpec {
	spec := managednet.NICSpec{MAC: mac, Bridge: gateway.ManagedBridge, Firewall: s.c.appliance()}
	if g.Ref.Kind == model.KindLXC {
		spec.Addr = netip.PrefixFrom(addr, s.net.Prefix.Bits())
	}
	return spec
}

// attach gives a consented guest its card: the gate of an expanding write
// must be open and nothing may hold the managed network. The attachment is
// saved before the card is added, so that a card never exists without the
// record of it; a card an attachment has lost goes back with its lease.
func (s *managedStep) attach(d managednet.Decision) {
	c := s.c
	owner := d.Owner
	ref, _ := model.ParseGuestRef(owner)
	g, ok := c.snap.Guest(ref)
	if !ok {
		return
	}
	if why := s.blocked(owner); why != "" {
		s.wait(owner, why)
		return
	}
	att, healing := s.attachments[owner]
	switch {
	case !healing:
		nic, free := managednet.FreeIndex(g)
		if !free {
			s.wait(owner, "the guest has a card at every index net0 to net31")
			return
		}
		var ok bool
		if att, ok = s.newAttachment(g, nic, d.Consent); !ok {
			return
		}
	case !s.keepLease(att):
		return
	case att.DetachAt != nil || att.DetachWhy != "":
		att.DetachAt, att.DetachWhy = nil, ""
		if err := c.e.d.Store.SaveAttachment(att); err != nil {
			c.problem(problemManagedCard, s.name(owner), fmt.Sprintf("saving the attachment: %v", err))
		} else {
			s.attachments[owner] = att
		}
	}
	if !s.guestFirewall(g, &att) {
		return
	}
	live, err := c.e.d.Guests.AddNIC(c.ctx, g, att.NIC, s.spec(g, att.MAC, att.Addr))
	if err != nil {
		c.problem(problemManagedCard, s.name(owner), err.Error())
		s.unverified[owner] = err.Error()
		return
	}
	if !healing || !c.e.attachedSaid[owner] {
		c.e.attachedSaid[owner] = true
		s.event(levelInfo, owner, fmt.Sprintf("%s is attached to the managed network: net%d at %s, by %s", s.name(owner), att.NIC, att.Addr, att.Consent))
	}
	switch {
	case live:
		delete(c.e.pendingCards, owner)
		s.addCard(ref, att)
	case !c.e.pendingCards[owner]:
		c.e.pendingCards[owner] = true
		s.event(levelInfo, owner, fmt.Sprintf("the card net%d of %s on the managed network waits for the restart of the guest", att.NIC, s.name(owner)))
	}
}

// blocked says why no card is added for owner now: the cycle may not write,
// the managed network is held, or the gate of an expanding write is closed;
// empty when one may be.
func (s *managedStep) blocked(owner string) string {
	switch {
	case s.writeHold != "":
		return s.writeHold
	case s.hold != "":
		return s.hold
	}
	if open, why := s.c.gateOpen("managed card for " + owner); !open {
		return why
	}
	return ""
}

// newAttachment leases an address for the card net<nic> of g and saves the
// attachment of it; a lease whose attachment could not be saved is given
// back, so that no address is held for a card that never comes.
func (s *managedStep) newAttachment(g model.Guest, nic int, consent string) (store.Attachment, bool) {
	c := s.c
	owner := g.Ref.String()
	lease, err := s.ipam.Allocate(owner, g.Identity, nic)
	if err != nil {
		s.leaseFailed(owner, err)
		return store.Attachment{}, false
	}
	att := store.Attachment{Owner: owner, Identity: g.Identity, NIC: nic, MAC: lease.MAC, Addr: lease.Addr, Consent: consent, Since: c.now}
	if err := c.e.d.Store.SaveAttachment(att); err != nil {
		if derr := c.e.d.Store.DeleteLease(lease.MAC); derr != nil {
			c.e.d.Log.Warn().Err(derr).Str("lease", lease.MAC).Msg("giving back the lease of an attachment that could not be saved failed")
		}
		c.problem(problemManagedCard, s.name(owner), fmt.Sprintf("saving the attachment: %v", err))
		s.wait(owner, "the attachment could not be saved: "+err.Error())
		return store.Attachment{}, false
	}
	s.attachments[owner] = att
	s.leases[lease.MAC] = lease
	return att, true
}

// keepLease makes sure the lease of an attachment whose card goes back is
// there: one that was lost is saved again for the same card, and a card
// whose lease cannot be saved again is not put back.
func (s *managedStep) keepLease(att store.Attachment) bool {
	c := s.c
	if _, ok := s.leases[strings.ToLower(att.MAC)]; ok {
		return true
	}
	l := store.Lease{MAC: att.MAC, Addr: att.Addr, Owner: att.Owner, Identity: att.Identity, NIC: att.NIC, Since: c.now}
	if err := c.e.d.Store.SaveLease(l, 0); err != nil {
		why := fmt.Sprintf("the lease of %s is gone and could not be saved again: %v", att.Addr, err)
		c.problem(problemManagedCard, s.name(att.Owner), why)
		s.unverified[att.Owner] = why
		return false
	}
	s.leases[strings.ToLower(att.MAC)] = l
	return true
}

// leaseFailed says that a guest got no lease: an exhausted subnet once as
// an event, and as a problem line while it stands.
func (s *managedStep) leaseFailed(owner string, err error) {
	c := s.c
	if !errors.Is(err, managednet.ErrExhausted) {
		c.problem(problemManagedCard, s.name(owner), err.Error())
		s.wait(owner, err.Error())
		return
	}
	c.problem("%v", err)
	s.wait(owner, err.Error())
	if !c.e.exhausted {
		c.e.exhausted = true
		s.event(levelError, "", err.Error())
	}
}

// addCard puts the card added live on the guest of the snapshot copy, so
// that the resolution of this cycle sees it with its lease.
func (s *managedStep) addCard(ref model.GuestRef, att store.Attachment) {
	for i := range s.c.snap.Guests {
		g := &s.c.snap.Guests[i]
		if g.Ref != ref {
			continue
		}
		if _, taken := g.NIC(att.NIC); taken {
			return
		}
		nic := model.NIC{Index: att.NIC, MAC: att.MAC, Bridge: gateway.ManagedBridge, Firewall: s.c.appliance(), Leased: att.Addr}
		if g.Ref.Kind == model.KindLXC {
			nic.Static = []netip.Addr{att.Addr}
		}
		g.NICs = append(slices.Clone(g.NICs), nic)
		slices.SortFunc(g.NICs, func(a, b model.NIC) int { return a.Index - b.Index })
	}
}

// keep ends a grace that runs for an attachment whose consent is back.
func (s *managedStep) keep(d managednet.Decision) {
	att, ok := s.attachments[d.Owner]
	if !ok || att.DetachAt == nil || s.writeHold != "" {
		return
	}
	att.DetachAt, att.DetachWhy = nil, ""
	if err := s.c.e.d.Store.SaveAttachment(att); err != nil {
		s.c.problem(problemManagedCard, s.name(d.Owner), fmt.Sprintf("saving the attachment: %v", err))
		return
	}
	s.attachments[d.Owner] = att
	s.event(levelInfo, d.Owner, fmt.Sprintf("the card net%d of %s on the managed network stays: its consent is back", att.NIC, s.name(d.Owner)))
}

// detach lets the grace of an attachment run and removes the card once it
// ended; the attachment and its lease go once the configuration of the
// guest no longer lists the card.
func (s *managedStep) detach(d managednet.Decision) {
	c := s.c
	att, ok := s.attachments[d.Owner]
	if !ok || d.DetachAt == nil {
		return
	}
	if s.writeHold != "" {
		s.heldDetach[d.Owner] = true
		return
	}
	ref, _ := model.ParseGuestRef(d.Owner)
	g, listed := c.snap.Guest(ref)
	if att.RemovedAt != nil {
		s.release(att)
		return
	}
	if att.DetachAt == nil || !att.DetachAt.Equal(*d.DetachAt) || att.DetachWhy != d.Why {
		started := att.DetachAt == nil
		at := *d.DetachAt
		att.DetachAt, att.DetachWhy = &at, d.Why
		if err := c.e.d.Store.SaveAttachment(att); err != nil {
			c.problem(problemManagedCard, s.name(d.Owner), fmt.Sprintf("saving the attachment: %v", err))
			return
		}
		s.attachments[d.Owner] = att
		if started {
			s.event(levelWarn, d.Owner, fmt.Sprintf("the card net%d of %s on the managed network goes at %s: %s", att.NIC, s.name(d.Owner), at.Format(time.RFC3339), d.Why))
		}
	}
	if c.now.Before(*att.DetachAt) {
		return
	}
	if listed && c.e.d.Guests != nil {
		if err := c.e.d.Guests.RemoveNIC(c.ctx, g, att.NIC); err != nil {
			c.problem(problemManagedCard, s.name(d.Owner), err.Error())
			return
		}
	}
	now := c.now
	att.RemovedAt = &now
	if err := c.e.d.Store.SaveAttachment(att); err != nil {
		c.problem(problemManagedCard, s.name(d.Owner), fmt.Sprintf("saving the attachment: %v", err))
		return
	}
	s.attachments[d.Owner] = att
	delete(c.e.pendingCards, d.Owner)
	s.event(levelInfo, d.Owner, fmt.Sprintf("the card net%d of %s was removed from the managed network; its lease stays until the configuration no longer lists the card",
		att.NIC, s.name(d.Owner)))
	s.release(att)
}

// release frees the lease of a removed card once the configuration no longer
// lists it, and then forgets the attachment.
func (s *managedStep) release(att store.Attachment) {
	c := s.c
	err := s.ipam.Release(att, c.snap.Guests)
	switch {
	case errors.Is(err, managednet.ErrCardPresent):
		return
	case err != nil:
		c.problem(problemManagedCard, s.name(att.Owner), err.Error())
		return
	}
	if !s.restoreFirewall(att) {
		return
	}
	if err := c.e.d.Store.DeleteAttachment(att.Owner); err != nil {
		c.problem(problemManagedCard, s.name(att.Owner), fmt.Sprintf("forgetting the attachment: %v", err))
		return
	}
	delete(s.attachments, att.Owner)
	delete(s.leases, att.MAC)
	s.event(levelInfo, att.Owner, fmt.Sprintf("%s is detached from the managed network; %s is free again", s.name(att.Owner), att.Addr))
}

// foreign acts on a card no attachment explains: the copy of a managed card
// on a guest that is to be attached is replaced by the guest's own card in
// one write, under the gate; any other is set link down where pco may write
// the guest, and reported where it may not. Every foreign card is a problem
// line while it is there.
func (s *managedStep) foreign(d managednet.Decision) {
	c := s.c
	switch d.Action {
	case managednet.ActionReplace, managednet.ActionLinkDown, managednet.ActionReport:
	default:
		return
	}
	ref, _ := model.ParseGuestRef(d.Owner)
	g, ok := c.snap.Guest(ref)
	if !ok {
		return
	}
	view := ForeignView{GuestView: *c.guestView(ref), NIC: d.NIC, MAC: d.MAC, Why: d.Why}
	c.problem(problemForeignCard, d.NIC, d.MAC, s.name(d.Owner), d.Why)
	switch d.Action {
	case managednet.ActionReplace:
		view.Action = ForeignReplaced
		if !s.replace(d, g) {
			s.instead(d, g, &view)
		}
	case managednet.ActionLinkDown:
		view.Action = ForeignLinkDown
		s.linkDown(d, g, &view)
	default:
		view.Action = ForeignReported
		s.foreignEvent(view, fmt.Sprintf("foreign card net%d (%s) of %s: %s", d.NIC, d.MAC, s.name(d.Owner), d.Why))
	}
	s.v.Foreign = append(s.v.Foreign, view)
}

// replace gives the guest its own card at the index of the copy: a lease,
// the attachment, then the one write that replaces the card. It reports
// whether the copy is gone or goes with the guest's restart; false leaves it
// to instead.
func (s *managedStep) replace(d managednet.Decision, g model.Guest) bool {
	c := s.c
	owner := d.Owner
	if why := s.blocked(owner); why != "" {
		s.wait(owner, why)
		return false
	}
	att, ok := s.attachments[owner]
	switch {
	case !ok:
		if att, ok = s.newAttachment(g, d.NIC, d.Consent); !ok {
			return false
		}
	case !s.keepLease(att):
		return false
	}
	if !s.guestFirewall(g, &att) {
		return false
	}
	live, err := c.e.d.Guests.AddNIC(c.ctx, g, d.NIC, s.spec(g, att.MAC, att.Addr))
	if err != nil {
		c.problem(problemManagedCard, s.name(owner), err.Error())
		s.unverified[owner] = err.Error()
		return false
	}
	c.e.attachedSaid[owner] = true
	s.event(levelWarn, owner, fmt.Sprintf("foreign card net%d (%s) of %s was replaced by the guest's own card with %s at %s", d.NIC, d.MAC, s.name(owner), att.MAC, att.Addr))
	if live {
		s.replaceCard(g.Ref, att)
	} else if !c.e.pendingCards[owner] {
		c.e.pendingCards[owner] = true
		s.event(levelInfo, owner, fmt.Sprintf("the card net%d of %s on the managed network waits for the restart of the guest", att.NIC, s.name(owner)))
	}
	return true
}

// instead answers a copy whose replacement cannot run now: its link goes
// down where pco may write the guest, and it is reported where it may not.
// The replacement follows in a cycle that can make it.
func (s *managedStep) instead(d managednet.Decision, g model.Guest, view *ForeignView) {
	if !s.writable[d.Owner] {
		view.Action = ForeignReported
		s.foreignEvent(*view, fmt.Sprintf("foreign card net%d (%s) of %s: %s", d.NIC, d.MAC, s.name(d.Owner), d.Why))
		return
	}
	view.Action = ForeignLinkDown
	s.linkDown(d, g, view)
}

// replaceCard puts the guest's own card in the place of the copy on the
// snapshot copy.
func (s *managedStep) replaceCard(ref model.GuestRef, att store.Attachment) {
	for i := range s.c.snap.Guests {
		g := &s.c.snap.Guests[i]
		if g.Ref != ref {
			continue
		}
		g.NICs = slices.DeleteFunc(slices.Clone(g.NICs), func(n model.NIC) bool { return n.Index == att.NIC })
	}
	s.addCard(ref, att)
}

// linkDown sets the link of a foreign card down. A container that refuses
// the option loses the card instead: the restrictive answer must take
// effect.
func (s *managedStep) linkDown(d managednet.Decision, g model.Guest, view *ForeignView) {
	c := s.c
	err := c.e.d.Guests.SetLinkDown(c.ctx, g, d.NIC)
	switch {
	case errors.Is(err, managednet.ErrPending):
		view.Why += "; the change waits for the restart of the guest"
		s.foreignEvent(*view, fmt.Sprintf("foreign card net%d (%s) of %s was set link down: %s", d.NIC, d.MAC, s.name(d.Owner), view.Why))
	case errors.Is(err, managednet.ErrLinkDownRefused) && g.Ref.Kind == model.KindLXC:
		if rerr := c.e.d.Guests.RemoveNIC(c.ctx, g, d.NIC); rerr != nil {
			c.problem(problemManagedCard, s.name(d.Owner), fmt.Sprintf("%v; removing the card instead: %v", err, rerr))
			return
		}
		view.Action = ForeignRemoved
		s.event(levelWarn, d.Owner, fmt.Sprintf("foreign card net%d (%s) of %s was removed, as the container refused link_down: %v", d.NIC, d.MAC, s.name(d.Owner), err))
	case err != nil:
		c.problem(problemManagedCard, s.name(d.Owner), err.Error())
	default:
		s.foreignEvent(*view, fmt.Sprintf("foreign card net%d (%s) of %s was set link down: %s", d.NIC, d.MAC, s.name(d.Owner), view.Why))
	}
}

// foreignEvent reports a foreign card once: not again while the last cycle
// showed it with the same action.
func (s *managedStep) foreignEvent(view ForeignView, msg string) {
	if slices.ContainsFunc(s.prev.Foreign, func(p ForeignView) bool {
		return p.GuestRef == view.GuestRef && p.NIC == view.NIC && strings.EqualFold(p.MAC, view.MAC) && p.Action == view.Action
	}) {
		return
	}
	s.event(levelWarn, view.String(), msg)
}

// noteAuto says whether automatic attaches run, and why not when they do
// not: the gate tag left the registered tags, or they could not be read.
func (s *managedStep) noteAuto() {
	c := s.c
	if c.settings.Managed.Consent != store.ConsentAuto {
		return
	}
	switch {
	case s.registered == nil:
		s.v.AutoStopped = autoUnreadable
		c.problem(problemAutoUnreadable)
	case !s.v.AutoAllowed:
		s.v.AutoStopped = fmt.Sprintf(problemAutoStopped, c.settings.GateTag)
		c.problem("%s", s.v.AutoStopped)
	}
}

// fill makes the views of the attachments, puts the leases on the cards of
// the snapshot copy and counts the free addresses.
func (s *managedStep) fill() {
	c := s.c
	for _, owner := range slices.SortedFunc(maps.Keys(s.attachments), model.CompareOwners) {
		att := s.attachments[owner]
		ref, _ := model.ParseGuestRef(owner)
		for i := range c.snap.Guests {
			g := &c.snap.Guests[i]
			if g.Ref != ref {
				continue
			}
			for j := range g.NICs {
				if g.NICs[j].Index == att.NIC && strings.EqualFold(g.NICs[j].MAC, att.MAC) && att.RemovedAt == nil {
					g.NICs[j].Leased = att.Addr
					// The configuration the guest runs with has the card:
					// it is live, also after a restart.
					delete(c.e.pendingCards, owner)
				}
			}
		}
		s.v.Attachments = append(s.v.Attachments, s.attachmentView(att))
	}
	s.v.Free = managednet.Available(s.net.Prefix, s.leases)
	if s.v.Free > 0 {
		c.e.exhausted = false
	}
}

// attachmentView says how an attachment stands.
func (s *managedStep) attachmentView(att store.Attachment) AttachmentView {
	c := s.c
	ref, _ := model.ParseGuestRef(att.Owner)
	v := AttachmentView{Attachment: att, Guest: c.guestView(ref), State: AttachmentAttached}
	g, listed := c.snap.Guest(ref)
	switch {
	case s.heldDetach[att.Owner]:
		v.State, v.Detail = AttachmentDetaching, s.writeHold
	case att.RemovedAt != nil:
		v.State, v.Detail = AttachmentDetachPending, att.DetachWhy+"; the card is removed once the configuration no longer lists it"
	case att.DetachAt != nil:
		v.State, v.Detail = AttachmentDetaching, fmt.Sprintf("goes at %s: %s", att.DetachAt.Format(time.RFC3339), att.DetachWhy)
	case listed && g.Node != c.e.d.Node:
		v.State, v.Detail = AttachmentMigrated, "on node "+g.Node
		s.migrated[att.Owner] = true
	case s.unverified[att.Owner] != "":
		v.State, v.Detail = AttachmentUnverified, s.unverified[att.Owner]
	case c.e.pendingCards[att.Owner]:
		v.State, v.Detail = AttachmentPendingReboot, "waits for the restart of the guest"
	case c.appliance() && s.grant[att.Owner] != "" && !s.writable[att.Owner]:
		v.State, v.GrantCmd = AttachmentNeedsGrant, s.grant[att.Owner]
	}
	return v
}

// events tells what changed against the last cycle: automatic attaches
// stopped or resumed, the vnet changed or back, the managed network refused
// or allowed again, a guest that migrated.
func (s *managedStep) events() {
	prev, v := s.prev, s.v
	switch {
	case v.AutoStopped != "" && v.AutoStopped != prev.AutoStopped:
		s.event(levelWarn, "", v.AutoStopped)
	case v.AutoStopped == "" && prev.AutoStopped != "" && prev.Enabled:
		s.event(levelInfo, "", fmt.Sprintf("automatic attaches resumed: the gate tag %s is registered as admin-only again", s.c.settings.GateTag))
	}
	switch {
	case !v.Vnet.OK && !slices.Equal(v.Vnet.Why, prev.Vnet.Why):
		s.event(levelError, gateway.ManagedBridge, fmt.Sprintf(problemVnetChanged, strings.Join(v.Vnet.Why, "; ")))
	case v.Vnet.OK && !prev.Vnet.OK && prev.Enabled:
		s.event(levelInfo, gateway.ManagedBridge, "the vnet "+gateway.ManagedBridge+" is as pco made it again")
	}
	switch {
	case v.Refused != "" && v.Refused != prev.Refused:
		s.event(levelError, "", fmt.Sprintf(problemManagedRefused, v.Refused))
	case v.Refused == "" && prev.Refused != "" && prev.Enabled:
		s.event(levelInfo, "", "the managed network can run on this node again")
	}
	for owner := range s.migrated {
		before := slices.ContainsFunc(prev.Attachments, func(a AttachmentView) bool { return a.Owner == owner && a.State == AttachmentMigrated })
		if ref, err := model.ParseGuestRef(owner); err == nil && !before {
			if g, ok := s.c.snap.Guest(ref); ok {
				s.event(levelWarn, owner, fmt.Sprintf("%s took its card on the managed network to node %s", s.name(owner), g.Node))
			}
		}
	}
}

// event records an event of the managed network about a guest, or about
// the network when owner is empty.
func (s *managedStep) event(level, owner, msg string) {
	ev := Event{At: s.c.now, Level: level, Kind: kindManaged, Subject: cmp.Or(owner, gateway.ManagedBridge), Message: msg}
	if owner != "" {
		ev.Guest = guestOf(owner)
	}
	s.c.events = append(s.c.events, ev)
}

// name names a guest with its name when the snapshot knows it.
func (s *managedStep) name(owner string) string {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return owner
	}
	return OwnerName(owner, s.c.guestView(ref))
}

// dhcp

// The DHCP server of the managed network asks the engine for the leases and
// tells it what it saw, between cycles; dhcpBook holds both, under its own
// lock, and each cycle brings it up to date with the leases it read.
type dhcpBook struct {
	mu sync.Mutex
	// loaded says that a cycle filled leases; before one did, a lookup
	// reads the store, at most once every leaseReadEvery, from readAt.
	loaded bool
	readAt time.Time
	leases map[string]dhcpLease // by MAC
	// seen is, by MAC, the last request of a lease's card; said, by MAC and
	// what it did, when an event last told of it; waitFrom, by owner, since
	// when the card of a running virtual machine is live without a request.
	seen     map[string]time.Time
	said     map[string]time.Time
	waitFrom map[string]time.Time
	// view and problem are what the server last said of itself.
	view    *DHCPView
	problem error
}

// dhcpLease is a lease as the server is given it.
type dhcpLease struct {
	addr  netip.Addr
	owner string
}

const (
	// awaitDHCP is how long the card of a running virtual machine may be
	// live without a DHCP request before the attachment says that it waits
	// for one.
	awaitDHCP          = 60 * time.Second
	detailAwaitingDHCP = "no DHCP request seen on the new card; see docs/networking.md for the snippet of your distribution"
	// lastDHCPEvery is how much later a request must be than the one its
	// lease records before a cycle writes it: a guest renews every 3.5 days,
	// and a flood of requests must not become a flood of writes.
	lastDHCPEvery = time.Hour
	// dhcpEventEvery is how often a card's DECLINE or RELEASE is told.
	dhcpEventEvery = 5 * time.Minute
	// leaseReadEvery bounds the reads of the store for the server before
	// the first cycle.
	leaseReadEvery = 5 * time.Second
	problemDHCP    = "the DHCP server is not listening on the managed leg: %v"
)

// DHCPLeases is the engine as the DHCP server of the managed network asks
// it; its methods are safe to call beside the cycles.
type DHCPLeases struct{ e *Engine }

// DHCPLeases returns what the DHCP server of the managed network asks.
func (e *Engine) DHCPLeases() DHCPLeases { return DHCPLeases{e: e} }

// Lookup returns the address of a MAC that has a lease. Before the first
// cycle the leases are read from the store, so that guests that boot with the
// node get theirs.
func (l DHCPLeases) Lookup(mac net.HardwareAddr) (netip.Addr, bool) {
	e := l.e
	b := &e.dhcp
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		if now := e.d.Now(); b.readAt.IsZero() || now.Sub(b.readAt) >= leaseReadEvery || now.Before(b.readAt) {
			b.readAt = now
			if leases, err := e.d.Store.Leases(); err != nil {
				e.d.Log.Debug().Err(err).Msg("the DHCP server could not read the leases")
			} else {
				b.fill(leases)
			}
		}
	}
	lease, ok := b.leases[mac.String()]
	return lease.addr, ok
}

// Seen records a request from the card of a lease at t.
func (l DHCPLeases) Seen(mac net.HardwareAddr, t time.Time) {
	b := &l.e.dhcp
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]time.Time{}
	}
	if last, ok := b.seen[mac.String()]; !ok || t.After(last) {
		b.seen[mac.String()] = t
	}
}

// Declined tells that the guest of a card found its address in use.
func (l DHCPLeases) Declined(mac net.HardwareAddr, t time.Time) {
	l.e.dhcpEvent(mac, t, "declined", levelWarn, "%s declined its managed address %s: the guest found it in use on the network")
}

// Released tells that the guest of a card gave its address up; the card
// keeps its lease.
func (l DHCPLeases) Released(mac net.HardwareAddr, t time.Time) {
	l.e.dhcpEvent(mac, t, "released", levelInfo, "%s released its managed address %s; the card keeps its lease")
}

// dhcpEvent tells what the guest of a card did, once per card and what in
// dhcpEventEvery.
func (e *Engine) dhcpEvent(mac net.HardwareAddr, t time.Time, what, level, format string) {
	b := &e.dhcp
	b.mu.Lock()
	lease, ok := b.leases[mac.String()]
	key := mac.String() + " " + what
	last, said := b.said[key]
	if !ok || said && t.Sub(last) < dhcpEventEvery && !t.Before(last) {
		b.mu.Unlock()
		return
	}
	if b.said == nil {
		b.said = map[string]time.Time{}
	}
	b.said[key] = t
	b.mu.Unlock()
	e.events.add(Event{At: t, Level: level, Kind: kindManaged, Subject: lease.owner, Guest: guestOf(lease.owner),
		Message: fmt.Sprintf(format, lease.owner, lease.addr)})
}

// NoteDHCP takes what the DHCP server of the managed network says of itself:
// nil while none runs, and why it does not listen when it does not.
func (e *Engine) NoteDHCP(v *DHCPView, problem error) {
	b := &e.dhcp
	b.mu.Lock()
	changed := (b.view == nil) != (v == nil) || b.view != nil && v != nil && b.view.Bound != v.Bound ||
		(b.problem == nil) != (problem == nil)
	b.view, b.problem = cloneP(v), problem
	b.mu.Unlock()
	if changed {
		e.Trigger()
	}
}

// fill makes the leases the server answers those given.
func (b *dhcpBook) fill(leases map[string]store.Lease) {
	b.leases = make(map[string]dhcpLease, len(leases))
	for mac, l := range leases {
		b.leases[strings.ToLower(mac)] = dhcpLease{addr: l.Addr, owner: l.Owner}
	}
	for mac := range b.seen {
		if _, ok := b.leases[mac]; !ok {
			delete(b.seen, mac)
		}
	}
	for key := range b.said {
		if mac, _, _ := strings.Cut(key, " "); b.leases[mac] == (dhcpLease{}) {
			delete(b.said, key)
		}
	}
}

// dhcp gives the server the leases of the cycle, writes when a card last
// asked, says which virtual machines wait for their first request, and puts
// the server's state into the view.
func (s *managedStep) dhcp() {
	c, b := s.c, &s.c.e.dhcp
	m := &c.st.Managed
	b.mu.Lock()
	if !m.Enabled {
		b.waitFrom = nil
		b.mu.Unlock()
		return
	}
	m.DHCP = cloneP(b.view)
	if b.problem != nil {
		c.problem(problemDHCP, b.problem)
	}
	if s.leases == nil {
		b.mu.Unlock()
		return
	}
	b.fill(s.leases)
	b.loaded = true
	seen := maps.Clone(b.seen)
	s.awaiting(b, seen)
	b.mu.Unlock()
	s.saveLastDHCP(seen)
}

// awaiting marks the attachments whose card has been live on a running
// virtual machine for awaitDHCP without a request, ever: a container has its
// address in its configuration and asks for none.
func (s *managedStep) awaiting(b *dhcpBook, seen map[string]time.Time) {
	c := s.c
	waitFrom := map[string]time.Time{}
	for i := range c.st.Managed.Attachments {
		a := &c.st.Managed.Attachments[i]
		ref, err := model.ParseGuestRef(a.Owner)
		if err != nil || ref.Kind != model.KindQEMU || a.State != AttachmentAttached {
			continue
		}
		mac := strings.ToLower(a.MAC)
		if g, ok := c.snap.Guest(ref); !ok || !g.Running {
			continue
		}
		if _, asked := seen[mac]; asked {
			continue
		}
		if l, ok := s.leases[mac]; ok && l.LastDHCP != nil {
			continue
		}
		from, ok := b.waitFrom[a.Owner]
		if !ok || c.now.Before(from) {
			from = c.now
		}
		waitFrom[a.Owner] = from
		if c.now.Sub(from) >= awaitDHCP {
			a.State, a.Detail = AttachmentAwaitingDHCP, detailAwaitingDHCP
		}
	}
	b.waitFrom = waitFrom
}

// saveLastDHCP writes to each lease when its card last asked, where that is
// lastDHCPEvery later than what it records. A lease changed since the cycle
// read it is written by a later cycle.
func (s *managedStep) saveLastDHCP(seen map[string]time.Time) {
	c := s.c
	for _, mac := range slices.Sorted(maps.Keys(seen)) {
		l, ok := s.leases[mac]
		at := seen[mac]
		if !ok || l.LastDHCP != nil && at.Sub(*l.LastDHCP) < lastDHCPEvery {
			continue
		}
		l.LastDHCP = &at
		if err := c.e.d.Store.SaveLease(l, l.Rev); err != nil {
			c.e.d.Log.Debug().Err(err).Str("lease", mac).Msg("writing when the card of a lease last asked DHCP failed")
			continue
		}
		s.leases[mac] = l
	}
}

// firewall

// errNotRead is what a read of the node's firewall that the managed step did
// not get to says.
var errNotRead = errors.New("not read in this cycle")

// noteBackend starts what the step records of the node's firewall for the
// filtered step of the cycle.
func (s *managedStep) noteBackend() *backendRead {
	mem := &s.c.e.filtered
	mem.fresh(s.c)
	mem.backend = backendRead{read: true, nodeErr: errNotRead, clusterErr: errNotRead, serviceErr: errNotRead}
	return &mem.backend
}

// guestFirewall writes, in the appliance, what isolates the card of att on
// g, before the card is added: the guest firewall as it was is recorded in
// the attachment first, so that a detach puts it back whatever happens
// after. False when the card must not be added.
func (s *managedStep) guestFirewall(g model.Guest, att *store.Attachment) bool {
	c := s.c
	if !c.appliance() {
		return true
	}
	fail := func(why string) bool {
		c.problem(problemManagedCard, s.name(att.Owner), why)
		s.unverified[att.Owner] = why
		return false
	}
	fw := s.firewall()
	if fw == nil {
		return fail("this daemon cannot write the firewall of guests, and no card is added without it")
	}
	if att.Firewall == nil {
		read, err := fw.Read(c.ctx, g, att.NIC)
		if err != nil {
			return fail("the firewall of the guest could not be read: " + err.Error())
		}
		before := managednet.Before(read.Options, len(read.Rules))
		att.Firewall = &before
		if err := c.e.d.Store.SaveAttachment(*att); err != nil {
			return fail(fmt.Sprintf("saving the attachment: %v", err))
		}
		s.attachments[att.Owner] = *att
	}
	if err := fw.Apply(c.ctx, g, c.firewallCard(s.net, g, *att)); err != nil {
		return fail("the firewall of the card could not be written: " + err.Error())
	}
	return true
}

// firewall is the writer of the guests' firewall under the comment of this
// install; nil where the daemon has none.
func (s *managedStep) firewall() *managednet.Firewall {
	if s.c.e.d.Firewall == nil {
		return nil
	}
	return managednet.NewFirewall(s.c.e.d.Firewall, s.c.install.ID)
}

// firewallCard is the card of att as pco writes its firewall: the leg of n on
// this node, the lease, and the ports the guest's routes publish.
func (c *cycleRun) firewallCard(n store.Network, g model.Guest, att store.Attachment) managednet.FirewallCard {
	owner := g.Ref.String()
	var ports []uint16
	for _, rt := range c.col.Routes {
		if rt.Guest != nil && rt.Owner() == owner && rt.Target.Port != 0 {
			ports = append(ports, rt.Target.Port)
		}
	}
	return managednet.FirewallCard{NIC: att.NIC, Leg: n.Legs[c.e.d.Node], Lease: att.Addr, Ports: slices.Compact(slices.Sorted(slices.Values(ports)))}
}

// firewalls keeps, in the appliance, the firewall of each attached guest's
// card as pco wrote it: the ports follow the guest's routes, and an ipset or
// rules that drifted are written again. A port is opened only while the
// cycle may write and the gate of an expanding write is open; closing one,
// and putting back what drifted, never waits. The guest firewall's options
// are left alone: an admin who turned it off is not overruled, filtered
// fails for the card instead. What it read is left for the filtered step of
// the cycle.
func (s *managedStep) firewalls() {
	c := s.c
	mem := &c.e.filtered
	mem.fresh(c)
	mem.attachments, mem.net = maps.Clone(s.attachments), s.net
	if !c.appliance() {
		return
	}
	fw := s.firewall()
	open := s.writeHold == ""
	if open {
		open, _ = c.gateOpen("managed firewall ports")
	}
	type job struct {
		g   model.Guest
		att store.Attachment
	}
	var jobs []job
	for _, owner := range slices.SortedFunc(maps.Keys(s.attachments), model.CompareOwners) {
		att := s.attachments[owner]
		ref, _ := model.ParseGuestRef(owner)
		g, ok := c.snap.Guest(ref)
		if !ok || att.RemovedAt != nil {
			continue
		}
		if nic, ok := g.NIC(att.NIC); !ok || nic.Bridge != gateway.ManagedBridge || !strings.EqualFold(nic.MAC, att.MAC) {
			continue
		}
		jobs = append(jobs, job{g: g, att: att})
	}
	results := make([]firewallRead, len(jobs))
	slots := make(chan struct{}, firewallConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			results[i] = s.keepFirewall(fw, j.g, j.att, open)
		}()
	}
	wg.Wait()
	for i, j := range jobs {
		r := results[i]
		mem.reads[j.att.Owner] = r
		if r.err != nil {
			c.problem(problemManagedCard, s.name(j.att.Owner), "its firewall: "+r.err.Error())
		}
	}
}

// keepFirewall reads the firewall of one attached guest's card and writes
// again what drifted, with the ports open may open. It returns the card as
// it stands written, and the firewall as read after any write.
func (s *managedStep) keepFirewall(fw *managednet.Firewall, g model.Guest, att store.Attachment, open bool) firewallRead {
	c := s.c
	card := c.firewallCard(s.net, g, att)
	if fw == nil {
		return firewallRead{card: card, err: errors.New("this daemon cannot read the firewall of guests")}
	}
	read, err := fw.Read(c.ctx, g, att.NIC)
	if err != nil {
		return firewallRead{card: card, err: err}
	}
	if !open {
		card.Ports = slices.DeleteFunc(card.Ports, func(p uint16) bool { return !slices.Contains(read.OpenPorts(card, fw.Comment()), p) })
	}
	if len(read.Drift(card, fw.Comment())) == 0 || att.Firewall == nil {
		return firewallRead{card: card, fw: &read}
	}
	if err := fw.Keep(c.ctx, g, card); err != nil {
		return firewallRead{card: card, fw: &read, err: err}
	}
	read, err = fw.Read(c.ctx, g, att.NIC)
	if err != nil {
		return firewallRead{card: card, err: err}
	}
	return firewallRead{card: card, fw: &read}
}

// restoreFirewall puts back, in the appliance, the guest firewall of a card
// that is gone, as the attachment recorded it: false while it could not,
// which keeps the attachment for the next cycle to try again. A guest that
// is gone took its firewall with it.
func (s *managedStep) restoreFirewall(att store.Attachment) bool {
	c := s.c
	if !c.appliance() || att.Firewall == nil {
		return true
	}
	ref, _ := model.ParseGuestRef(att.Owner)
	g, ok := c.snap.Guest(ref)
	if !ok {
		return true
	}
	fw := s.firewall()
	if fw == nil {
		c.problem(problemManagedCard, s.name(att.Owner), "this daemon cannot put its firewall back")
		return false
	}
	if err := fw.Restore(c.ctx, g, att.NIC, att.Firewall); err != nil {
		c.problem(problemManagedCard, s.name(att.Owner), "putting its firewall back: "+err.Error())
		return false
	}
	s.event(levelInfo, att.Owner, fmt.Sprintf("the firewall of %s is as it was before its card on the managed network", s.name(att.Owner)))
	return true
}
