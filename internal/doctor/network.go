package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// NetworkEnv is what the doctor reads of the gateway and the managed network
// beside the state. An Env without it has those checks made from the state
// alone.
type NetworkEnv interface {
	// NetnsPinned is nil when the gateway namespace is pinned at
	// /run/netns/pco.
	NetnsPinned(ctx context.Context) error
	// UnitProperty is a property of a unit as systemd has it loaded, as
	// systemctl show prints its value: empty for an ExecStop the unit does
	// not have, the units it is ordered after for After.
	UnitProperty(ctx context.Context, unit, property string) (string, error)
	// SDNPending is the SDN with the changes that wait to be applied.
	SDNPending(ctx context.Context) (pve.SDN, error)
	// PconetIPv6 reports whether IPv6 is on on the bridge pconet; false when
	// there is no such bridge.
	PconetIPv6(ctx context.Context) (bool, error)
	// HostTable is nil when the table inet pco_host is loaded, and
	// gateway.ErrNotLoaded when it is not.
	HostTable(ctx context.Context) error
	// ServicePool is how many service addresses were given out, released
	// ones included, and how many the pool holds.
	ServicePool(ctx context.Context) (used, size int, err error)
	// Pins are the neighbours the gateway was last given to pin, with when.
	Pins(ctx context.Context) ([]store.Pin, time.Time, error)
}

const (
	netnsUnit = "pco-netns.service"
	hostTable = "pco_host"

	fixNetns   = "systemctl restart pco-netns"
	fixOldUnit = "reinstall the package, then systemctl daemon-reload"
	fixProxmox = "the proxmox check says why Proxmox does not answer"

	// legsFull is how many networks short of gateway.MaxMark the networks
	// check warns.
	legsFull = 2
	// fewFree is how few free addresses of the managed subnet make the
	// managed check warn.
	fewFree = 10
	// shownReasons is how many reasons of the filtered predicate a finding
	// names.
	shownReasons = 3
)

// gatewayFaults are the starts of the problem lines of a gateway namespace
// that could not be brought back, or not checked.
var gatewayFaults = []string{
	"the gateway namespace was changed outside pco and could not be brought back",
	"checking the gateway namespace: ",
}

// checkNetworks checks the gateway, the legs and the managed network, from
// the state and from what env tells of them. A node that reaches every guest
// directly has none of them; app is the part of an appliance.
func checkNetworks(ctx context.Context, st engine.State, env Env, app ApplianceEnv) []Finding {
	net, _ := env.(NetworkEnv)
	appliance := st.Profile == store.ProfileAppliance
	var out []Finding
	if !appliance && net != nil {
		if f, found := checkNetns(ctx, st, env, net); found {
			out = append(out, f)
		}
	}
	if len(st.Networks) == 0 && !st.Managed.Enabled {
		return out
	}
	out = append(out, checkLegs(st)...)
	out = append(out, checkMarks(st))
	if net != nil {
		out = append(out, checkServicePool(ctx, net))
	}
	m := st.Managed
	if !m.Enabled {
		return out
	}
	out = append(out, checkManaged(m)...)
	out = append(out, checkVnet(ctx, m, net, appliance), checkDHCP(m))
	if !appliance && net != nil {
		out = append(out, checkPconet(ctx, net))
	}
	out = append(out, checkManagedGuests(m)...)
	out = append(out, checkForeignCards(m)...)
	if appliance {
		out = append(out, checkFiltered(m), checkManagedFirewall(m))
		if m.PoolGrant != "" && app != nil {
			out = append(out, checkPoolGrant(ctx, m, app)...)
		}
	}
	return out
}

// checkNetns checks the gateway namespace of a host and its unit: pinned and
// as pco left it while a network is attached, started at boot before pco, and
// never torn down by a stop. A node without a network whose unit is not
// enabled has nothing to check.
func checkNetns(ctx context.Context, st engine.State, env Env, net NetworkEnv) (Finding, bool) {
	const check = "netns"
	networks := len(st.Networks) > 0
	enabled, enabledErr := env.UnitEnabled(ctx, netnsUnit)
	if !networks && (enabledErr != nil || !enabled) {
		return Finding{}, false
	}
	stop, stopErr := net.UnitProperty(ctx, netnsUnit, "ExecStop")
	oldUnit := stopErr == nil && strings.TrimSpace(stop) != ""
	gone := func(detail string) (Finding, bool) {
		if oldUnit {
			return fail(check, detail+"; the loaded "+netnsUnit+" has an ExecStop line, which tears the gateway down on restart", fixOldUnit+" and "+fixNetns), true
		}
		return fail(check, detail, fixNetns), true
	}
	var tableErr error
	if networks {
		if err := net.NetnsPinned(ctx); err != nil {
			return gone(fmt.Sprintf("the gateway namespace pco is not pinned (%v): the legs and the service addresses are gone", err))
		}
		if i := slices.IndexFunc(st.Problems, isGatewayFault); i >= 0 {
			return gone(st.Problems[i])
		}
		tableErr = net.HostTable(ctx)
		if errors.Is(tableErr, gateway.ErrNotLoaded) {
			return gone("the table inet pco_host is not loaded: the node's own services are not kept from the managed network and the transit link")
		}
	}
	switch {
	case oldUnit:
		return fail(check, "the loaded pco-netns.service has an ExecStop line: an old pco-netns.service tears the gateway down on restart", fixOldUnit), true
	case tableErr != nil:
		return warn(check, "the table inet pco_host could not be read: "+tableErr.Error(), "nft list table inet pco_host"), true
	case enabledErr != nil:
		return warn(check, "systemd did not say whether "+netnsUnit+" is enabled: "+enabledErr.Error(), "systemctl is-enabled "+netnsUnit), true
	case !enabled:
		return warn(check, netnsUnit+" is not enabled: after a boot no gateway is up for the guests behind the legs", "systemctl enable pco-netns"), true
	}
	after, afterErr := net.UnitProperty(ctx, daemonUnit, "After")
	switch {
	case stopErr != nil || afterErr != nil:
		return warn(check, "systemd did not say how "+netnsUnit+" and "+daemonUnit+" are loaded: "+cmpErr(stopErr, afterErr).Error(),
			"systemctl show "+netnsUnit+" "+daemonUnit), true
	case !slices.Contains(strings.Fields(after), netnsUnit):
		return warn(check, "the loaded "+daemonUnit+" is not ordered after "+netnsUnit+": a unit file from before an upgrade",
			"systemctl daemon-reload; reinstall the package if it stays"), true
	case !networks:
		return ok(check, netnsUnit+" is enabled and starts before pco; no network is attached"), true
	}
	return ok(check, "the gateway namespace pco is pinned and as pco left it, and "+netnsUnit+" starts it at boot before pco"), true
}

// cmpErr is the first error that is not nil.
func cmpErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func isGatewayFault(line string) bool {
	return slices.ContainsFunc(gatewayFaults, func(prefix string) bool { return strings.HasPrefix(line, prefix) })
}

// checkLegs has a finding for the leg of every network on this node: up, up
// with a problem, or not up. The state shows the legs of this node only.
func checkLegs(st engine.State) []Finding {
	var out []Finding
	for _, n := range st.Networks {
		check, fix := "leg "+n.ID, "pco network show "+n.ID
		if n.ID == gateway.ManagedBridge {
			fix = "pco managed status"
		}
		iface, on := gateway.LegIface(n.Mark), segment(n.Network)
		for _, l := range n.Legs {
			switch {
			case !l.Up:
				out = append(out, fail(check, fmt.Sprintf("%s on %s is not up: %s", iface, on, l.Problem), fix))
			case l.Problem != "":
				out = append(out, warn(check, fmt.Sprintf("%s is up with %s on %s, but %s", iface, l.Addr, on, l.Problem), fix))
			default:
				out = append(out, ok(check, fmt.Sprintf("%s is up with %s on %s", iface, l.Addr, on)))
			}
		}
	}
	return out
}

// segment names the bridge and the VLAN of a network.
func segment(n store.Network) string {
	if n.VLAN == 0 {
		return n.Bridge
	}
	return fmt.Sprintf("%s VLAN %d", n.Bridge, n.VLAN)
}

// checkMarks warns when the networks near the most a node reaches: each takes
// a mark and a block of source ports of its own.
func checkMarks(st engine.State) Finding {
	const check = "networks"
	used := len(st.Networks)
	detail := fmt.Sprintf("%d of the %d networks a node reaches are attached", used, gateway.MaxMark)
	if used >= gateway.MaxMark-legsFull {
		return warn(check, detail, "detach a network that serves nothing: pco network list shows what each serves")
	}
	return ok(check, detail)
}

// checkServicePool warns at 80 % of the pool of service addresses given out
// and fails at 95 %: an address is never given twice, so the pool only
// shrinks.
func checkServicePool(ctx context.Context, net NetworkEnv) Finding {
	const check = "service pool"
	used, size, err := net.ServicePool(ctx)
	switch {
	case err != nil:
		return warn(check, "the pool of service addresses could not be read: "+err.Error(), fixProblems)
	case size <= 0:
		return warn(check, "the pool of service addresses was not read: it has no address", fixProblems)
	}
	detail := fmt.Sprintf("%d of %d service addresses given out (%d %%)", used, size, used*100/size)
	const (
		full = "; when none is left, a new target behind a leg is held"
		fix  = "a target behind a leg keeps its service address while its address stays; " +
			"find the targets whose addresses change often, since no address is given twice"
	)
	switch {
	case used*100 >= 95*size:
		return fail(check, detail+full, fix)
	case used*100 >= 80*size:
		return warn(check, detail+full, fix)
	}
	return ok(check, detail)
}

// checkManaged says whether the managed network runs on this node, and what
// keeps guests from getting a card: a finding for every condition that holds,
// so that one does not hide another.
func checkManaged(m engine.ManagedView) []Finding {
	const (
		check   = "managed"
		options = "registered-tags in the options of the datacenter"
	)
	var out []Finding
	if m.Refused != "" {
		out = append(out, fail(check, "the managed network cannot run on this node: "+m.Refused+"; nothing is attached until it can, and attached guests keep their cards",
			"put right what it names; pco managed status shows it"))
	}
	if m.Leg != nil && !m.Leg.Up {
		out = append(out, fail(check, "the leg of the managed network is not up: "+m.Leg.Problem, "pco managed status"))
	}
	if m.AutoStopped != "" {
		out = append(out, warn(check, m.AutoStopped, "add the gate tag to "+options))
	}
	if m.Consent != store.ConsentAuto && !m.TagHonoured {
		out = append(out, warn(check, "the tag "+managednet.Tag+" is not registered as admin-only, so it gives no guest consent; only approvals do",
			"add "+managednet.Tag+" to "+options+", or approve guests with pco managed approve"))
	}
	if m.Free < fewFree {
		out = append(out, warn(check, fmt.Sprintf("%d addresses of %s are free", m.Free, m.Subnet), "detach the guests that no longer need a card on the managed network"))
	}
	if len(out) > 0 {
		return out
	}
	consent := "consent " + m.Consent + ", the tag " + managednet.Tag + " is honoured"
	if m.Consent == store.ConsentAuto {
		consent = "consent auto, the gate tag is registered"
	}
	return []Finding{ok(check, fmt.Sprintf("%s; %s attached; %d addresses of %s free", consent, count(len(m.Attachments), "guest", "guests"), m.Free, m.Subnet))}
}

// checkVnet fails while the vnet is not as pco made it, and warns about
// changes of others to the SDN that wait to be applied: an apply on any
// node, pco's own among them, applies them along.
func checkVnet(ctx context.Context, m engine.ManagedView, net NetworkEnv, appliance bool) Finding {
	const check = "vnet"
	isolate := "on"
	if appliance {
		isolate = "off"
	}
	if !m.Vnet.OK {
		return fail(check, "the vnet pconet was changed outside pco: "+strings.Join(m.Vnet.Why, "; ")+"; nothing is attached until it is as pco made it",
			"put pconet back as pco made it: zone pco of type simple, isolate-ports "+isolate+", no subnet; pco managed status shows what differs")
	}
	detail := "zone pco of type simple, isolate-ports " + isolate + ", no subnet"
	if net == nil {
		return ok(check, detail)
	}
	sdn, err := net.SDNPending(ctx)
	if appliance && isDenied(err) {
		return ok(check, detail+"; the changes of the SDN that wait to be applied were not read: pco's token may not read them ("+err.Error()+")")
	}
	if err != nil {
		fix := fixProxmox
		if appliance {
			fix = fixAPI
		}
		return warn(check, "the changes of the SDN that wait to be applied could not be read: "+err.Error(), fix)
	}
	if others := othersPending(sdn); len(others) > 0 {
		return warn(check, "the SDN has changes that are not applied and are not pco's: "+strings.Join(others, "; ")+
			"; the next apply, on any node, applies them along", "apply them, or undo them, in Datacenter > SDN")
	}
	return ok(check, detail+"; no change of the SDN waits to be applied")
}

// isDenied reports whether Proxmox refused a privilege the token lacks, as
// opposed to rejecting the token.
func isDenied(err error) bool {
	var apiErr *pve.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

// othersPending names the changes of the SDN that wait to be applied, but
// those of pco's zone and vnet: the vnet check says how those differ.
func othersPending(sdn pve.SDN) []string {
	return pve.SDN{
		Zones:   slices.DeleteFunc(slices.Clone(sdn.Zones), func(z pve.SDNZone) bool { return z.Name == setup.ManagedZone }),
		Vnets:   slices.DeleteFunc(slices.Clone(sdn.Vnets), func(v pve.SDNVnet) bool { return v.Name == setup.ManagedVnet }),
		Subnets: slices.DeleteFunc(slices.Clone(sdn.Subnets), func(s pve.SDNSubnet) bool { return s.Vnet == setup.ManagedVnet }),
	}.Pending()
}

// checkPconet fails while the bridge of the managed network carries IPv6 on
// a host: its link-local address would reach the node's services.
func checkPconet(ctx context.Context, net NetworkEnv) Finding {
	const check = "pconet"
	on, err := net.PconetIPv6(ctx)
	switch {
	case err != nil:
		return warn(check, "whether IPv6 is off on the bridge pconet could not be read: "+err.Error(), "sysctl net.ipv6.conf.pconet.disable_ipv6")
	case on:
		return fail(check, "IPv6 is on on the bridge pconet: managed guests reach the node at its link-local address", fixNetns)
	}
	return ok(check, "IPv6 is off on the bridge pconet")
}

// checkDHCP fails while the DHCP server of the managed guests is not bound.
// What it did not answer is only counted.
func checkDHCP(m engine.ManagedView) Finding {
	const check = "dhcp"
	switch {
	case m.DHCP == nil:
		return warn(check, "the DHCP server of the managed network has not said how it is", "journalctl -u pco says why it does not start")
	case !m.DHCP.Bound:
		return fail(check, "the DHCP server of the managed network is not bound: virtual machines on pconet get no address",
			"journalctl -u pco says why; systemctl restart pco")
	}
	unknown := "no request from a MAC without a lease"
	if n := m.DHCP.Unknown; n > 0 {
		unknown = count(n, "request", "requests") + " from MACs without a lease since the start, not answered"
	}
	return ok(check, "bound since "+m.DHCP.Since.UTC().Format(time.RFC3339)+"; "+unknown)
}

// The starts of the reasons a consented guest waits for its card that the
// admin has to act on.
const (
	waitGrant    = "needs-grant"
	waitFirewall = "needs-firewall-confirmation: "
)

// checkManagedGuests has a finding for every guest of the managed network
// that is not plainly attached, and for every one that waits for its card on
// a grant or a confirmation of the admin.
func checkManagedGuests(m engine.ManagedView) []Finding {
	var out []Finding
	for _, a := range m.Attachments {
		if f, found := checkAttachment(a); found {
			out = append(out, f)
		}
	}
	for _, w := range m.Waiting {
		owner := w.String()
		check, who := "managed guest "+owner, engine.OwnerName(owner, &w.GuestView)
		switch {
		case strings.HasPrefix(w.Why, waitFirewall):
			why, _, _ := strings.Cut(strings.TrimPrefix(w.Why, waitFirewall), "; run ")
			out = append(out, warn(check, who+" waits for a card: "+why,
				"pco managed approve "+owner+" --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest"))
		case strings.HasPrefix(w.Why, waitGrant):
			out = append(out, warn(check, who+" waits for a card: pco's token may not change the cards of the guest",
				grantFix(strings.TrimPrefix(strings.TrimPrefix(w.Why, waitGrant), ": "), owner)))
		}
	}
	return out
}

// checkAttachment says what holds an attachment back, if anything does.
func checkAttachment(a engine.AttachmentView) (Finding, bool) {
	check, who := "managed guest "+a.Owner, engine.OwnerName(a.Owner, a.Guest)
	card := fmt.Sprintf("net%d", a.NIC)
	const restart = "restart the guest: a virtual machine without network hotplug "
	switch a.State {
	case engine.AttachmentPendingReboot:
		return warn(check, who+": "+card+" is added and takes effect at the next restart of the guest", restart+"takes a new card only then"), true
	case engine.AttachmentAwaitingDHCP:
		return warn(check, fmt.Sprintf("%s: %s waits for the guest to ask DHCP for %s", who, card, a.Addr),
			"have the guest ask DHCP on that card, as its distribution configures it"), true
	case engine.AttachmentNeedsGrant:
		return warn(check, who+": pco's token may not change the cards of the guest", grantFix(a.GrantCmd, a.Owner)), true
	case engine.AttachmentDetaching:
		return warn(check, who+": "+card+" "+a.Detail, "give the guest its consent back before then to keep the card"), true
	case engine.AttachmentDetachPending:
		return warn(check, who+": "+card+": "+a.Detail, restart+"drops the card only then"), true
	case engine.AttachmentUnverified:
		return warn(check, who+": "+card+" could not be added or checked in the last cycle: "+a.Detail, fixProblems), true
	case engine.AttachmentMigrated:
		node := strings.TrimPrefix(a.Detail, "on node ")
		return fail(check, who+" is on node "+node+", not on this node: it keeps its card and its address, and the managed network does not reach it there",
			"migrate it back: a guest on the managed network cannot move to another node yet"), true
	}
	return Finding{}, false
}

// grantFix is the pveum line that lets pco change the cards of a guest, or
// what it grants when the state has no line.
func grantFix(line, owner string) string {
	if line != "" {
		return line
	}
	return "grant role PCOManaged on the guest (" + owner + ") to pco's user and its token"
}

// checkForeignCards has a finding for every card on the managed network that
// no attachment explains and that is still there: reported, where pco may
// not change the guest, or set link down.
func checkForeignCards(m engine.ManagedView) []Finding {
	var out []Finding
	for _, f := range m.Foreign {
		owner := f.String()
		check := "foreign card " + owner
		detail := fmt.Sprintf("net%d (%s) of %s is on pconet and no attachment explains it: %s", f.NIC, f.MAC, engine.OwnerName(owner, &f.GuestView), f.Why)
		switch f.Action {
		case engine.ForeignReported:
			out = append(out, fail(check, detail, "take the card off the guest, or grant pco role PCOManaged on it so that pco sets it link down"))
		case engine.ForeignLinkDown:
			out = append(out, warn(check, detail, "take the card off the guest; with consent, pco replaces it by a card of its own"))
		}
	}
	return out
}

// checkFiltered says whether the guests of the appliance's managed network
// are proven isolated by their firewall: the filtered level of their routes.
func checkFiltered(m engine.ManagedView) Finding {
	const (
		check    = "filtered pconet"
		observed = "; the routes on pconet are proven at observed"
	)
	f := m.Filtered
	switch {
	case f == nil:
		return warn(check, "not judged yet", "wait for the next cycle")
	case !f.Offered:
		why := "the backend " + f.Backend + " installs no guest rules"
		if len(f.Why) > 0 {
			why = f.Why[0]
		}
		return warn(check, "the filtered level is not offered on this node: "+why+observed, "set nftables: 0 in the firewall options of the node")
	case !f.Holds:
		return fail(check, "the guests on pconet are not proven isolated: "+strings.Join(f.Why[:min(len(f.Why), shownReasons)], "; ")+observed,
			"pco managed status lists every reason")
	}
	return ok(check, "the guests on pconet are isolated by their firewall ("+f.Backend+")")
}

// checkManagedFirewall fails while the appliance's node cannot isolate the
// managed guests: the nftables backend, the datacenter firewall off or
// pve-firewall not running, which the cycle reads and refuses the managed
// network for. It says whether the node can isolate them, not whether the
// rules of each guest are in force: the appliance cannot read the node's
// live chains.
func checkManagedFirewall(m engine.ManagedView) Finding {
	const check = "managed firewall"
	if m.Refused != "" {
		return fail(check, m.Refused+"; the routes on pconet are proven at observed",
			"put right what it names; nothing is attached until it is, and attached guests keep their cards")
	}
	return ok(check, "the node can isolate the managed guests: the datacenter firewall is on, and pve-firewall runs on the iptables backend; "+
		"whether the rules of each guest are in force is not read here, filtered pconet says what the cycle proved")
}

// checkPoolGrant names every guest of the pool whose cards pco's token may
// change that has no consent for the managed network: the grant reaches it
// all the same, though pco changes no guest without consent. A guest that is
// attached or waits, carries a registered tag cf-tunnel-managed, or that the
// gate tag admits under consent auto has consent.
func checkPoolGrant(ctx context.Context, m engine.ManagedView, app ApplianceEnv) []Finding {
	const check = "pool grant"
	guests, err := app.Guests(ctx)
	if err != nil {
		return []Finding{warn(check, "the guests could not be read: "+err.Error(), fixAPI)}
	}
	consented := make(map[string]bool)
	for _, a := range m.Attachments {
		consented[a.Owner] = true
	}
	for _, w := range m.Waiting {
		consented[w.String()] = true
	}
	hasConsent := func(g pve.Resource) bool {
		ref := model.GuestRef{Kind: g.Kind, VMID: g.VMID}
		return consented[ref.String()] ||
			(m.TagHonoured && slices.Contains(g.Tags, managednet.Tag)) ||
			(m.Consent == store.ConsentAuto && m.GateTag != "" && slices.Contains(g.Tags, m.GateTag))
	}
	self := app.Self().VMID
	var out []Finding
	for _, g := range guests {
		if g.Pool != m.PoolGrant || g.Template || g.VMID == self || hasConsent(g) {
			continue
		}
		ref := model.GuestRef{Kind: g.Kind, VMID: g.VMID}
		who := engine.OwnerName(ref.String(), &engine.GuestView{GuestRef: ref, Name: g.Name})
		out = append(out, ok(check+" "+ref.String(),
			who+" is in pool "+m.PoolGrant+": pco's token can change its cards and its firewall; no consent for the managed network today"))
	}
	if len(out) == 0 {
		return []Finding{ok(check, "every guest of pool "+m.PoolGrant+" has consent for the managed network")}
	}
	return out
}
