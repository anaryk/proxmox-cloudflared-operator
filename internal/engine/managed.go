package engine

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
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
	switch {
	case err != nil:
		return fmt.Sprintf("the firewall options of node %s could not be read: %v", node, err)
	case opts.NFTables:
		return fmt.Sprintf("the firewall of node %s runs on the nftables backend (nftables: 1), which installs no guest rules; "+
			"set nftables: 0 in its firewall options", node)
	}
	dc, err := c.e.d.ClusterFirewall(c.ctx)
	switch {
	case err != nil:
		return fmt.Sprintf("the firewall options of the datacenter could not be read: %v", err)
	case !dc.Enabled:
		return "the datacenter firewall is off; no guest rule is in force until it is turned on"
	}
	state, err := c.e.d.NodeService(c.ctx, firewallUnit)
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
