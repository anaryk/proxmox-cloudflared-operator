package engine

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	otherMAC     = "bc:24:11:00:00:09"
	macChanged   = "MAC changed from bc:24:11:00:00:01 to bc:24:11:00:00:09"
	aliceHolds   = "delegated: alice@pve holds VM.Config.Network"
	approveWebTo = "; pco guest approve qemu/101"
)

var (
	vmbr1     = resolve.Segment{Bridge: "vmbr1"}
	gatewayIP = netip.MustParseAddr("10.0.0.1")
	held503   = &planner.IngressRule{Hostname: "www.example.com", Service: "http_status:503"}
)

// observing is an engine in enforce mode that serves at observed, whose
// resolver proves www.example.com of qemu/101 at observed on vmbr1, which is
// acknowledged.
func observing(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.settings(func(s *store.Settings) { s.IdentityMinimum = string(resolve.LevelObserved) })
	e.res.setLevel("www.example.com", resolve.LevelObserved)
	e.res.proveOn(vmbr1)
	require.NoError(t, e.store.SaveSegment(store.Segment{Bridge: "vmbr1", AcknowledgedAt: t0, By: "cli"}))
	return e
}

func heldLine(n string, causes string) string {
	return n + " at observed " + map[bool]string{true: "is", false: "are"}[n == "1 route"] + " held, by cause: " + causes +
		"; pco routes says why each is held"
}

func requireServedAtObserved(t *testing.T, st State) {
	t.Helper()
	www := route(st, "www.example.com")
	require.Equal(t, planner.StateActive, www.State, www.Reason)
	require.Equal(t, "observed", www.Level)
	require.Equal(t, "http://10.0.0.11:8080", www.Service)
}

func requireHeld(t *testing.T, st State, reason string) {
	t.Helper()
	www := route(st, "www.example.com")
	require.Equal(t, planner.StateUnreachable, www.State)
	require.Equal(t, "observed", www.Level, "the level stays what resolution proved")
	require.Equal(t, reason, www.Reason)
	require.Empty(t, www.Service)
	if www.Rule != nil {
		require.Equal(t, held503, www.Rule)
	}
}

func webWaits(why []string, macs []string, addrs ...netip.Addr) []UnapprovedGuest {
	return []UnapprovedGuest{{
		GuestView: GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:101", Hostnames: []string{"www.example.com"},
		Why: why, MACs: macs, Addresses: addrs,
	}}
}

// Admission waits before resolution and only in mode approve; the observed
// rules hold after it, in either mode, and neither asks more of a guest than
// its own rules do.
func TestTheTwoMechanismsAreKeptApart(t *testing.T) {
	t.Run("mode tag serves a guest at port without an approval", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/102", Identity: "uuid:102"}))

		st := e.cycle()

		require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
		require.Empty(t, st.Unapproved)
		require.Empty(t, st.Problems)
	})
	t.Run("at observed on an acknowledged segment it is served without an approval too", func(t *testing.T) {
		e := observing(t)
		require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/102", Identity: "uuid:102"}))

		st := e.cycle()

		requireServedAtObserved(t, st)
		require.Empty(t, st.Unapproved)
		require.Empty(t, st.Problems)
		require.Equal(t, []string{"www.example.com"}, e.recordNames())
	})
	t.Run("mode approve still waits before resolution", func(t *testing.T) {
		e := observing(t)
		e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })

		st := e.cycle()

		require.Empty(t, st.Routes)
		require.Equal(t, webWaits([]string{"admission mode approve"}, nil), st.Unapproved)
	})
}

func TestAnUnacknowledgedSegmentHoldsItsRoutes(t *testing.T) {
	e := observing(t)
	e.res.proveOn(resolve.Segment{Bridge: "vmbr1", VLAN: 20})

	st := e.cycle()

	requireHeld(t, st, "segment vmbr1 VLAN 20 is not acknowledged; pco segment acknowledge vmbr1:20")
	require.Empty(t, e.recordNames())
	require.Equal(t, []string{heldLine("1 route", "segment not acknowledged 1")}, st.Problems)
	require.Empty(t, st.Unapproved, "no approval of the guest releases it")
	require.Equal(t, []SegmentView{
		{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: t0},
		{Bridge: "vmbr1", VLAN: 20, Routes: 1},
	}, st.Segments)

	drain(e)
	require.NoError(t, e.eng.AcknowledgeSegment(t.Context(), "vmbr1", 20))
	requireTriggered(t, e)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	requireServedAtObserved(t, st)
	require.Empty(t, st.Problems)
	require.Equal(t, []SegmentView{
		{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: t0},
		{Bridge: "vmbr1", VLAN: 20, Acknowledged: true, AcknowledgedAt: t0, Routes: 1},
	}, st.Segments)

	require.NoError(t, e.eng.RevokeSegment(t.Context(), "vmbr1", 20))
	requireTriggered(t, e)
	e.clock.advance(61 * time.Second)
	st = e.cycle()

	requireHeld(t, st, "segment vmbr1 VLAN 20 is not acknowledged; pco segment acknowledge vmbr1:20")
	require.Equal(t, withSentinel(), e.rules(), "the tunnel no longer sends it to the guest")
}

func TestTheFirstServiceAtObservedPinsTheMAC(t *testing.T) {
	e := observing(t)

	st := e.cycle()

	requireServedAtObserved(t, st)
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, testMAC, claims["www.example.com"].MAC)
	require.Equal(t, 1, eventsContaining(e, "pinned MAC bc:24:11:00:00:01 for www.example.com"))

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, 1, eventsContaining(e, "pinned MAC"), "once")
}

func TestANewMACWaitsForApprovalAndIsPinnedOnceApproved(t *testing.T) {
	e := observing(t)
	e.cycle()
	e.res.answerFrom("www.example.com", otherMAC)
	e.clock.advance(10 * time.Second)

	st := e.cycle()

	requireHeld(t, st, macChanged+"; approve the guest to accept it")
	require.Equal(t, webWaits([]string{macChanged}, []string{otherMAC}), st.Unapproved)
	require.Equal(t, []string{heldLine("1 route", "MAC changed 1")}, st.Problems)
	require.Equal(t, []string{"www.example.com"}, e.recordNames(), "the record stays for its claim")
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, testMAC, claims["www.example.com"].MAC, "the pin stays until an approval")

	a, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101", []string{otherMAC}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{otherMAC}, a.MACs)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	requireServedAtObserved(t, st)
	require.Empty(t, st.Unapproved)
	claims, err = e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, otherMAC, claims["www.example.com"].MAC)
	require.Equal(t, 1, eventsContaining(e, "pinned MAC bc:24:11:00:00:09 for www.example.com in place of bc:24:11:00:00:01, "+
		"as the approval of qemu/101 allows"))
}

// A MAC approved for one identity is not approved for a guest re-created
// under the same VMID.
func TestAnApprovedMACOfAnotherIdentityDoesNotRepin(t *testing.T) {
	e := observing(t)
	e.cycle()
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:old", MACs: []string{otherMAC}}))
	e.res.answerFrom("www.example.com", otherMAC)
	e.clock.advance(10 * time.Second)

	st := e.cycle()

	requireHeld(t, st, macChanged+"; approve the guest to accept it")
}

func TestARenumberUnderTheSameMACNeedsNoApproval(t *testing.T) {
	e := observing(t)
	e.cycle()
	e.res.moveTo("www.example.com", netip.MustParseAddr("10.0.0.12"))
	e.clock.advance(10 * time.Second)

	st := e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateActive, www.State, www.Reason)
	require.Equal(t, "http://10.0.0.12:8080", www.Service)
	require.Empty(t, st.Unapproved)
	require.Empty(t, st.Problems)
	require.Zero(t, eventsContaining(e, "MAC changed"))
}

func TestADelegatedGuestWaitsForApproval(t *testing.T) {
	delegated := func(t *testing.T) *env {
		t.Helper()
		e := observing(t)
		e.acc.grant("alice@pve", "/vms/101", "NetAdmin", "VM.Config.Network")
		st := e.cycle()
		requireHeld(t, st, aliceHolds+approveWebTo)
		require.Equal(t, webWaits([]string{aliceHolds}, []string{testMAC}), st.Unapproved)
		require.Equal(t, []string{heldLine("1 route", "delegated guest 1")}, st.Problems)
		return e
	}
	t.Run("released by an approval of its MAC", func(t *testing.T) {
		e := delegated(t)
		_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101", []string{testMAC}, nil)
		require.NoError(t, err)
		e.clock.advance(10 * time.Second)

		st := e.cycle()

		requireServedAtObserved(t, st)
		require.Empty(t, st.Unapproved)
	})
	t.Run("an approval without its MAC does not release it", func(t *testing.T) {
		e := delegated(t)
		require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:101"}))
		e.clock.advance(10 * time.Second)

		requireHeld(t, e.cycle(), aliceHolds+approveWebTo)
	})
	t.Run("released when the grant leaves at the next refresh", func(t *testing.T) {
		e := delegated(t)
		e.acc.revokeAll()
		e.clock.advance(30 * time.Second)
		requireHeld(t, e.cycle(), aliceHolds+approveWebTo)

		e.clock.advance(31 * time.Second)
		st := e.cycle()

		requireServedAtObserved(t, st)
		require.Empty(t, st.Unapproved)
	})
}

func TestWhoMakesAGuestDelegated(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env)
		why   string // empty: served
	}{
		{name: "nobody"},
		{name: "a user with the privilege elsewhere", setup: func(e *env) {
			e.acc.grant("alice@pve", "/vms/102", "NetAdmin", "VM.Config.Network")
		}},
		{name: "a user without the privilege", setup: func(e *env) {
			e.acc.grant("alice@pve", "/vms/101", "PVEVMUser", "VM.Audit", "VM.Console", "VM.PowerMgmt")
		}},
		{name: "an admin", setup: func(e *env) {
			e.acc.grant("admin@pve", "/", "Administrator", "Sys.Modify", "VM.Config.Network")
		}},
		{name: "pco's own user", setup: func(e *env) {
			e.acc.grant("pco@pve", "/", "NetAdmin", "VM.Config.Network")
		}},
		{name: "two users", setup: func(e *env) {
			e.acc.grant("bob@pve", "/vms", "NetAdmin", "VM.Config.Network")
			e.acc.grant("alice@pve", "/vms/101", "NetAdmin", "VM.Config.Network")
		}, why: "delegated: alice@pve, bob@pve hold VM.Config.Network"},
		{name: "a pool grant on the guest's pool", setup: func(e *env) {
			e.acc.grant("alice@pve", "/pool/web", "NetAdmin", "VM.Config.Network")
		}, why: aliceHolds},
		{name: "a pool grant past a narrower role on the guest", setup: func(e *env) {
			e.acc.grant("alice@pve", "/vms/101", "PVEAuditor", "VM.Audit")
			e.acc.grant("alice@pve", "/pool/web", "NetAdmin", "VM.Config.Network")
		}, why: aliceHolds},
		{name: "a pool grant on another pool", setup: func(e *env) {
			e.acc.grant("alice@pve", "/pool/db", "NetAdmin", "VM.Config.Network")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := observing(t)
			g := guest(101, "web-1", "www.example.com -> :8080")
			g.Pool = "web"
			e.inv.set(snapshot(g))
			if tt.setup != nil {
				tt.setup(e)
			}

			st := e.cycle()

			if tt.why == "" {
				requireServedAtObserved(t, st)
				require.Empty(t, st.Unapproved)
				return
			}
			requireHeld(t, st, tt.why+approveWebTo)
			require.Equal(t, []string{tt.why}, st.Unapproved[0].Why)
		})
	}
}

func TestUnreadableAccessControlMakesEveryObservedGuestWait(t *testing.T) {
	const unreadable = "the access control could not be read"
	tests := []struct {
		name  string
		setup func(e *env)
		cause string
	}{
		{name: "an error", setup: func(e *env) { e.acc.fail(errors.New("connection refused")) }, cause: "connection refused"},
		{
			name: "no Sys.Audit on /access", setup: func(e *env) { e.acc.setPerms("/access") },
			cause: "pco's token does not hold Sys.Audit on /access, so Proxmox lists only part of its access control",
		},
		{
			name: "no Sys.Audit on /access/groups", setup: func(e *env) { e.acc.setPerms("/access/groups", "Group.Allocate") },
			cause: "pco's token does not hold Sys.Audit on /access/groups, so Proxmox lists only part of its access control",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := observing(t)
			e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "api", "api.example.com -> :8080")))
			tt.setup(e)

			st := e.cycle()

			requireHeld(t, st, unreadable+approveWebTo)
			require.Equal(t, webWaits([]string{unreadable}, []string{testMAC}), st.Unapproved)
			require.Equal(t, planner.StateActive, route(st, "api.example.com").State, "a route at port is not touched")
			require.Equal(t, []string{
				heldLine("1 route", "access control unreadable 1"),
				"the access control of Proxmox could not be read since 2026-10-01T12:00:00Z; " +
					"every guest's observed routes wait for approval: " + tt.cause,
			}, st.Problems)

			_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101", []string{testMAC}, nil)
			require.NoError(t, err)
			e.clock.advance(10 * time.Second)
			requireServedAtObserved(t, e.cycle())
		})
	}
}

// What was read last stands for five minutes, so that one failed read does
// not hold every guest.
func TestTheLastAccessControlStandsForFiveMinutes(t *testing.T) {
	e := observing(t)
	e.acc.grant("alice@pve", "/vms/102", "NetAdmin", "VM.Config.Network")
	requireServedAtObserved(t, e.cycle())
	e.acc.fail(errors.New("connection refused"))

	e.clock.advance(61 * time.Second)
	st := e.cycle()
	requireServedAtObserved(t, st)
	require.Empty(t, st.Problems)

	e.clock.advance(4 * time.Minute)
	st = e.cycle()
	requireHeld(t, st, "the access control could not be read"+approveWebTo)
	require.Contains(t, st.Problems, "the access control of Proxmox could not be read since 2026-10-01T12:01:01Z; "+
		"every guest's observed routes wait for approval: connection refused")

	e.acc.fail(nil)
	e.clock.advance(61 * time.Second)
	st = e.cycle()
	requireServedAtObserved(t, st)
	require.Empty(t, st.Problems)
}

func TestTheAccessControlIsReadEveryMinute(t *testing.T) {
	e := observing(t)
	e.cycle()
	require.Equal(t, 1, e.acc.aclReads(), "at the first cycle")
	e.clock.advance(30 * time.Second)
	e.cycle()
	require.Equal(t, 1, e.acc.aclReads())
	e.clock.advance(30 * time.Second)
	e.cycle()
	require.Equal(t, 2, e.acc.aclReads())
}

func TestWithoutTheAccessControlEveryGuestIsDelegated(t *testing.T) {
	e := observing(t)
	e.acc = nil
	e.eng = e.newEngine()

	st := e.cycle()

	requireHeld(t, st, "the access control could not be read"+approveWebTo)
	require.Equal(t, []string{heldLine("1 route", "access control unreadable 1")}, st.Problems)
}

// gatewayOf puts the gateway on vmbr0 of the node.
func gatewayOf(snap inventory.Snapshot, gw netip.Addr) inventory.Snapshot {
	snap.Nodes[0].Ifaces = []pve.NodeIface{{Name: "vmbr0", Type: "bridge", Gateway: gw}}
	return snap
}

func TestASoftDeniedAddressWaitsForAnAllowance(t *testing.T) {
	const why = "address 10.0.0.1 is the gateway of node pve1"
	e := observing(t)
	e.inv.set(gatewayOf(snapshot(guest(101, "web-1", "www.example.com -> :8080")), gatewayIP))
	e.res.moveTo("www.example.com", gatewayIP)

	st := e.cycle()

	requireHeld(t, st, why+approveWebTo)
	require.Equal(t, webWaits([]string{why}, nil, gatewayIP), st.Unapproved)
	require.Equal(t, []string{heldLine("1 route", "soft-denied address 1")}, st.Problems)

	_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101", nil, []netip.Addr{gatewayIP})
	require.NoError(t, err)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateActive, www.State, www.Reason)
	require.Equal(t, "http://10.0.0.1:8080", www.Service)
}

func TestWhatIsSoftDenied(t *testing.T) {
	resolverIP := netip.MustParseAddr("10.0.0.53")
	ownGateway, ownResolver := netip.MustParseAddr("10.0.0.254"), netip.MustParseAddr("10.0.0.250")
	e := observing(t)
	e.inv.set(gatewayOf(snapshot(guest(101, "web-1", "www.example.com -> :8080")), gatewayIP))
	e.acc.dns[testNode] = pve.NodeDNS{Servers: []netip.Addr{resolverIP}}
	e.ownSoft = func() ([]netip.Addr, []netip.Addr, error) {
		return []netip.Addr{ownGateway}, []netip.Addr{ownResolver}, nil
	}
	e.eng = e.newEngine()

	e.cycle()

	soft, err := e.store.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, []store.SoftEntry{
		{Addr: gatewayIP, Why: "gateway of node pve1", LastSeen: t0},
		{Addr: resolverIP, Why: "resolver of node pve1", LastSeen: t0},
		{Addr: ownResolver, Why: "resolver of the appliance", LastSeen: t0},
		{Addr: ownGateway, Why: "gateway of the appliance", LastSeen: t0},
	}, soft.Entries)
	for _, addr := range []netip.Addr{resolverIP, ownGateway, ownResolver} {
		e.res.moveTo("www.example.com", addr)
		e.clock.advance(10 * time.Second)
		require.Equal(t, planner.StateUnreachable, route(e.cycle(), "www.example.com").State, addr)
	}
}

func TestAnAppliancesOwnAddressesThatCannotBeReadAreSaid(t *testing.T) {
	e := observing(t)
	e.ownSoft = func() ([]netip.Addr, []netip.Addr, error) { return nil, nil, errors.New("no default route") }
	e.eng = e.newEngine()

	st := e.cycle()

	require.Contains(t, st.Problems, "reading the gateway and resolvers of the appliance: no default route; the ones seen before stay soft-denied")
}

func TestSoftEntriesAgeOutAfterThirtyDays(t *testing.T) {
	e := observing(t)
	e.inv.set(gatewayOf(snapshot(guest(101, "web-1", "www.example.com -> :8080")), gatewayIP))
	e.res.moveTo("www.example.com", gatewayIP)
	e.cycle()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))

	e.clock.advance(29 * 24 * time.Hour)
	requireHeld(t, e.cycle(), "address 10.0.0.1 is the gateway of node pve1"+approveWebTo)

	e.clock.advance(2 * 24 * time.Hour)
	st := e.cycle()

	www := route(st, "www.example.com")
	require.Equal(t, planner.StateActive, www.State, www.Reason)
	soft, err := e.store.SoftDeny()
	require.NoError(t, err)
	require.Empty(t, soft.Entries)
}

// The time an entry was seen is written again only once it is an hour old,
// not every cycle.
func TestASoftEntrySeenAgainIsNotWrittenEveryCycle(t *testing.T) {
	e := observing(t)
	e.inv.set(gatewayOf(snapshot(guest(101, "web-1", "www.example.com -> :8080")), gatewayIP))
	e.cycle()
	e.clock.advance(30 * time.Minute)
	e.cycle()
	soft, err := e.store.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, t0, soft.Entries[0].LastSeen)

	e.clock.advance(31 * time.Minute)
	e.cycle()
	soft, err = e.store.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, t0.Add(61*time.Minute), soft.Entries[0].LastSeen)
}

func TestTheGatewayOfAnSDNSubnetIsNeverServed(t *testing.T) {
	e := observing(t)
	e.acc.subnets = []pve.Subnet{
		{Vnet: "v1", Prefix: netip.MustParsePrefix("10.0.0.0/24"), Gateway: gatewayIP},
		{Vnet: "v2", Prefix: netip.MustParsePrefix("10.9.0.0/24")},
	}

	e.cycle()

	why, denied := e.res.lastDeny().Check(gatewayIP)
	require.True(t, denied)
	require.Equal(t, "gateway of SDN subnet 10.0.0.0/24 of vnet v1", why)
	_, soft := e.res.lastDeny().Soft(gatewayIP)
	require.False(t, soft)
}

// Subnets that cannot be read again stay denied as they were read last, and
// the state says so where it matters.
func TestTheSubnetsReadLastStayDenied(t *testing.T) {
	e := observing(t)
	e.acc.subnets = []pve.Subnet{{Vnet: "v1", Prefix: netip.MustParsePrefix("10.0.0.0/24"), Gateway: gatewayIP}}
	e.cycle()
	e.acc.mu.Lock()
	e.acc.subnetsErr = errors.New("no sdn")
	e.acc.mu.Unlock()
	e.clock.advance(61 * time.Second)

	st := e.cycle()

	require.Contains(t, st.Problems, "reading the SDN subnets or the resolvers of the nodes: no sdn; the ones read before stay denied")
	_, denied := e.res.lastDeny().Check(gatewayIP)
	require.True(t, denied)

	e.settings(func(s *store.Settings) { s.IdentityMinimum = "port" })
	e.clock.advance(10 * time.Second)
	require.False(t, hasProblem(e.cycle(), "SDN"), "at port nothing of it is said")
}

func TestARouteAtPortNeverPinsOrWaits(t *testing.T) {
	for _, minimum := range []string{"port", "observed"} {
		t.Run("minimum "+minimum, func(t *testing.T) {
			e := newEnv(t)
			e.enforce()
			e.settings(func(s *store.Settings) { s.IdentityMinimum = minimum })
			e.res.proveOn(resolve.Segment{Bridge: "vmbr9"})
			e.acc.grant("alice@pve", "/vms/101", "NetAdmin", "VM.Config.Network")
			e.inv.set(gatewayOf(snapshot(guest(101, "web-1", "www.example.com -> :8080")), guestAddr))

			st := e.cycle()

			www := route(st, "www.example.com")
			require.Equal(t, planner.StateActive, www.State, www.Reason)
			require.Equal(t, "port", www.Level)
			require.Empty(t, st.Unapproved)
			require.Empty(t, st.Problems)
			claims, err := e.store.Claims()
			require.NoError(t, err)
			require.Empty(t, claims["www.example.com"].MAC)
			require.Empty(t, st.Segments, "a segment is seen at observed only")
		})
	}
}

// The observed rules are of the guest whose identity was proven, also for a
// manual route that names it.
func TestAManualRouteThatNamesAGuestFollowsTheObservedRules(t *testing.T) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	e := observing(t)
	e.inv.set(snapshot(guest(101, "web-1")))
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "www.example.com", ManualID: "www", Source: model.SourceManual, Guest: &ref,
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080},
	}))
	e.acc.grant("alice@pve", "/vms/101", "NetAdmin", "VM.Config.Network")

	st := e.cycle()

	requireHeld(t, st, aliceHolds+approveWebTo)
	require.Equal(t, []string{"www.example.com"}, st.Unapproved[0].Hostnames)
}

func TestSeveralCausesAreCountedEach(t *testing.T) {
	e := observing(t)
	e.inv.set(gatewayOf(snapshot(
		guest(101, "web-1", "www.example.com -> :8080"),
		guest(102, "api", "api.example.com -> :8080"),
	), gatewayIP))
	e.res.setLevel("api.example.com", resolve.LevelObserved)
	e.res.moveTo("api.example.com", gatewayIP)
	e.acc.grant("alice@pve", "/vms", "NetAdmin", "VM.Config.Network")

	st := e.cycle()

	require.Equal(t, []string{heldLine("2 routes", "delegated guest 2, soft-denied address 1")}, st.Problems)
	api := route(st, "api.example.com")
	require.Equal(t, aliceHolds+"; address 10.0.0.1 is the gateway of node pve1; pco guest approve qemu/102", api.Reason)
}
