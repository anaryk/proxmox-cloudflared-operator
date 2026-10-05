package main

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func qemu(vmid int, name string) *engine.GuestView {
	return &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: vmid}, Name: name}
}

// someClaims has a hostname for each state a claim can be in.
func someClaims() []engine.ClaimView {
	missing := t0.Add(-30 * time.Second)
	return []engine.ClaimView{
		{Hostname: "api.example.com", Holder: "manual/api", Since: t0.Add(-48 * time.Hour), State: engine.ClaimServing, Waiting: []engine.ClaimantView{}},
		{
			Hostname: "old.example.com", Holder: "qemu/104", Guest: qemu(104, "old"), Since: t0.Add(-24 * time.Hour),
			MissingSince: &missing, State: engine.ClaimHeld, Waiting: []engine.ClaimantView{},
		},
		{
			Hostname: "www.example.com", Holder: "qemu/101", Guest: qemu(101, "web-1"), Since: t0.Add(-72 * time.Hour), State: engine.ClaimConflict,
			Waiting: []engine.ClaimantView{
				{Owner: "qemu/102", Guest: qemu(102, "web-2"), Since: t0.Add(-time.Hour)},
				{Owner: "lxc/300", Guest: &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 300}}, Since: t0},
			},
		},
	}
}

// claimsState is a daemon where qemu/101 serves www.example.com and qemu/102
// asks for it too.
func claimsState() engine.State {
	st := healthyState()
	st.Routes = append(st.Routes, routeView("www.example.com", "qemu/102", planner.StateConflict, "", "", "hostname is held by qemu/101"))
	return st
}

func daemonWithClaims(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: claimsState(), claims: someClaims()}
	return newRunner(t, serveFake(t, e)), e
}

func TestClaimsListGolden(t *testing.T) {
	r, e := daemonWithClaims(t)

	res := r.run("", "claims", "list")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "claims_list.golden", res.out)
	require.Empty(t, e.called())
}

func TestClaimsListWithNone(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "claims", "list")

	require.NoError(t, res.err)
	require.Equal(t, "No claims.\n", res.out)
}

func TestClaimsListJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"state":"held","hostname":"a.example.com","holder":"qemu/1","futureField":{"z":1}}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/claims": {200, raw}}))

	res := r.run("", "--json", "claims", "list")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

func TestClaimsResolveShowsTheMoveAndAsks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		in    string
		args  []string
		moved bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "n\n", nil, false},
		{"an empty answer", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithClaims(t)

			res := r.tty().run(tt.in, append([]string{"claims", "resolve", "WWW.example.com.", "qemu/102"}, tt.args...)...)

			require.Contains(t, res.out, "www.example.com is held by qemu/101 (web-1) since 2026-09-28T14:00:00+02:00.\n"+
				"Resolving hands it to qemu/102 (web-2); qemu/101 (web-1) waits for it from then on, in the place in line its claim gives it.\n"+
				"From the next cycle qemu/102 holds it, and serves it once its address is verified: "+
				"pco diagnose www.example.com shows how that goes.\n")
			if tt.moved {
				require.NoError(t, res.err)
				require.Equal(t, []string{"resolve www.example.com qemu/102"}, e.called(), "the name in its normal form")
				require.True(t, strings.HasSuffix(res.out, "The claim on www.example.com is now held by qemu/102.\n"), res.out)
			} else {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called())
			}
			if len(tt.args) == 0 {
				require.Equal(t, "Move www.example.com to qemu/102? [y/N] ", res.errOut)
			}
		})
	}
}

func TestClaimsResolveGolden(t *testing.T) {
	r, _ := daemonWithClaims(t)

	res := r.tty().run("y\n", "claims", "resolve", "www.example.com", "lxc/300")

	require.NoError(t, res.err)
	requireGolden(t, "claims_resolve.golden", res.out+"--- stderr\n"+res.errOut)
}

// When there is nothing to move, it says so, and nothing is asked or sent.
func TestClaimsResolveWithNothingToMoveSendsNothing(t *testing.T) {
	for _, tt := range []struct {
		name, host, owner, out string
	}{
		{"a hostname nobody holds", "nope.example.com", "qemu/102", "Nobody holds a claim on nope.example.com; there is nothing to move.\n"},
		{"the holder itself", "www.example.com", "qemu/101", "qemu/101 (web-1) holds www.example.com already; there is nothing to move.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithClaims(t)

			res := r.tty().run("y\n", "claims", "resolve", tt.host, tt.owner)

			require.NoError(t, res.err)
			require.Equal(t, tt.out, res.out)
			require.Empty(t, res.errOut, "nothing was asked")
			require.Empty(t, e.called(), "nothing was sent")
		})
	}
}

// An owner that does not claim the hostname is the daemon's to refuse.
func TestClaimsResolveToAnOwnerThatDoesNotClaimIt(t *testing.T) {
	r, e := daemonWithClaims(t)
	e.resolveErr = fmt.Errorf("%w: qemu/109 no longer claims www.example.com: it has no route for it and does not name it", engine.ErrRefused)

	res := r.tty().run("y\n", "claims", "resolve", "www.example.com", "qemu/109")

	require.EqualError(t, res.err, "refused: qemu/109 no longer claims www.example.com: it has no route for it and does not name it")
	require.Equal(t, []string{"resolve www.example.com qemu/109"}, e.called())
}

// What a move does not bring about at once is said: nobody serves the
// hostname until the new holder routes it or is approved, and nothing is
// published while the daemon only observes or holds, also when it holds
// before it knows any tunnel.
func TestClaimsResolveSaysWhenNobodyWillServeTheHostname(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(st *engine.State)
		saying string
	}{
		{"a holder that waits for approval", func(st *engine.State) {
			st.Routes = st.Routes[:len(st.Routes)-1]
			st.Unapproved = []engine.UnapprovedGuest{{GuestView: *qemu(102, "web-2"), Identity: "uuid:102", Hostnames: []string{"www.example.com"}}}
		}, "qemu/102 waits for approval: nobody serves www.example.com until it is approved (pco guest approve qemu/102).\n"},
		{"a holder that only names it", func(st *engine.State) { st.Routes = st.Routes[:len(st.Routes)-1] },
			"qemu/102 names www.example.com without a route for it: nobody serves it until qemu/102 routes it.\n"},
		{"a daemon that only observes", func(st *engine.State) { st.Mode = engine.ModeObserve },
			"The daemon only observes: the claim moves now, but nothing is published until pco apply.\n"},
		{"a daemon that holds", func(st *engine.State) {
			st.Hold = "no writer identity; run pco setup"
			for i := range st.Tunnels {
				st.Tunnels[i].Held, st.Tunnels[i].Unchecked, st.Tunnels[i].Verified = "not checked in the last cycle: "+st.Hold, true, false
			}
		}, "The daemon holds (no writer identity; run pco setup): the claim moves now, " +
			"but qemu/102 serves www.example.com only once the daemon stops holding.\n"},
		{"a daemon that holds before it knows a tunnel", func(st *engine.State) {
			st.Hold = "the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are"
			st.Tunnels, st.Connectors = []engine.TunnelView{}, []connector.Status{}
		}, "The daemon holds (the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are): " +
			"the claim moves now, but qemu/102 serves www.example.com only once the daemon stops holding.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := claimsState()
			tt.state(&st)
			e := &fakeEngine{state: st, claims: someClaims()}
			r := newRunner(t, serveFake(t, e))

			res := r.run("", "claims", "resolve", "www.example.com", "qemu/102", "--yes")

			require.NoError(t, res.err)
			require.Contains(t, res.out, tt.saying)
			require.NotContains(t, res.out, "From the next cycle")
		})
	}
}

func TestClaimsResolveRefusesWhatIsNoHostname(t *testing.T) {
	r, e := daemonWithClaims(t)

	res := r.run("", "claims", "resolve", "not a host", "qemu/102")

	require.ErrorContains(t, res.err, `"not a host" is not a hostname`)
	require.Empty(t, e.called())
	require.Error(t, r.run("", "claims", "resolve", "www.example.com").err)
	require.Error(t, r.run("", "claims", "resolve").err)
}

// someApprovals has an approval that matches, one of a guest re-created since
// and one of a guest the daemon does not see.
func someApprovals() []engine.ApprovalView {
	return []engine.ApprovalView{
		{Owner: "qemu/101", Guest: qemu(101, "web-1"), Identity: "uuid:101", Current: "uuid:101", Matches: true},
		{Owner: "qemu/102", Guest: qemu(102, "db-1"), Identity: "uuid:old", Current: "uuid:102"},
		{Owner: "lxc/300", Guest: &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 300}}, Identity: "uuid:300"},
	}
}

func approvalState() engine.State {
	st := healthyState()
	st.Unapproved = []engine.UnapprovedGuest{
		{GuestView: *qemu(103, "new-1"), Identity: "uuid:103", Hostnames: []string{"new.example.com", "www.new.example.com"}},
		{GuestView: *qemu(104, ""), Identity: "uuid:104", Hostnames: []string{"db.example.com"}},
	}
	return st
}

func TestGuestListGolden(t *testing.T) {
	e := &fakeEngine{state: approvalState(), approvals: someApprovals()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "guest", "list")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "guest_list.golden", res.out)
	require.Empty(t, e.called())
}

func TestGuestListWithNothing(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "guest", "list")

	require.NoError(t, res.err)
	require.Equal(t, "No guest is approved.\nNo guest waits for approval.\n", res.out)
}

func TestGuestListJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"matches":true,"owner":"qemu/101","identity":"uuid:101"}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/approvals": {200, raw}}))

	res := r.run("", "--json", "guest", "list")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

// An approval shows what the guest would publish, sends the identity it
// showed, and says what was approved and what that does in the mode the
// daemon is in.
func TestGuestApprove(t *testing.T) {
	r, e := daemonWith(t, approvalState())

	res := r.runReader(unreadable{t}, "guest", "approve", "qemu/103")

	require.NoError(t, res.err)
	require.Equal(t, "qemu/103 (new-1) waits for approval in identity uuid:103; approved, it publishes new.example.com, www.new.example.com.\n"+
		"Approved qemu/103 in identity uuid:103.\n"+
		"From the next cycle its routes no longer wait for an approval.\n", res.out)
	require.Empty(t, res.errOut, "nothing is asked")
	require.Equal(t, []string{"approve qemu/103 identity=uuid:103"}, e.called(), "the identity that was shown")
}

func TestGuestApproveInModeTag(t *testing.T) {
	e := &fakeEngine{state: healthyState(), mode: "tag"}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "guest", "approve", "qemu/101")

	require.NoError(t, res.err)
	require.Equal(t, "Approved qemu/101 in identity uuid:101.\n"+
		"The admission mode is tag: the approval matters only for its routes at observed until the mode is approve.\n", res.out)
	require.Equal(t, []string{"approve qemu/101"}, e.called(), "no identity was shown, so none is sent")
}

// observedWaitState has qemu/101 wait at observed for three reasons.
func observedWaitState() engine.State {
	st := healthyState()
	st.Unapproved = []engine.UnapprovedGuest{{
		GuestView: *qemu(101, "web-1"), Identity: "uuid:101", Hostnames: []string{"www.example.com"},
		Why: []string{
			"MAC changed from bc:24:11:00:00:01 to bc:24:11:00:00:09",
			"delegated: alice@pve holds VM.Config.Network",
			"address 10.0.0.1 is the gateway of node pve1",
		},
		MACs: []string{"bc:24:11:00:00:09"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
	}}
	return st
}

// An approval of a guest that waits at observed says why it waits and what
// the approval records, and sends what was shown.
func TestGuestApproveOfAGuestThatWaitsAtObserved(t *testing.T) {
	const why = "qemu/101 (web-1) waits for approval in identity uuid:101; approved, it publishes www.example.com.\n" +
		"It waits because:\n" +
		"  MAC changed from bc:24:11:00:00:01 to bc:24:11:00:00:09\n" +
		"  delegated: alice@pve holds VM.Config.Network\n" +
		"  address 10.0.0.1 is the gateway of node pve1\n"
	t.Run("as shown", func(t *testing.T) {
		e := &fakeEngine{state: observedWaitState(), mode: "tag"}
		r := newRunner(t, serveFake(t, e))

		res := r.runReader(unreadable{t}, "guest", "approve", "qemu/101")

		require.NoError(t, res.err)
		require.Equal(t, why+
			"Approving it records MAC bc:24:11:00:00:09 and allows address 10.0.0.1.\n"+
			"Approved qemu/101 in identity uuid:101.\n"+
			"From the next cycle its routes no longer wait for an approval.\n", res.out)
		require.Equal(t, []string{"approve qemu/101 identity=uuid:101 macs=[bc:24:11:00:00:09] addresses=[10.0.0.1]"}, e.called())
	})
	t.Run("with an address the state did not list", func(t *testing.T) {
		r, e := daemonWith(t, observedWaitState())

		res := r.run("", "guest", "approve", "qemu/101", "--allow-address", "10.0.0.53", "--allow-address", "10.0.0.1")

		require.NoError(t, res.err)
		require.Contains(t, res.out, "Approving it records MAC bc:24:11:00:00:09 and allows addresses 10.0.0.1, 10.0.0.53.\n")
		require.Equal(t, []string{"approve qemu/101 identity=uuid:101 macs=[bc:24:11:00:00:09] addresses=[10.0.0.1 10.0.0.53]"}, e.called())
	})
	t.Run("an address only", func(t *testing.T) {
		r, e := daemonWith(t, healthyState())

		res := r.run("", "guest", "approve", "qemu/101", "--allow-address", "10.0.0.53")

		require.NoError(t, res.err)
		require.Equal(t, "Approving it allows address 10.0.0.53.\n"+
			"Approved qemu/101 in identity uuid:101.\n"+
			"From the next cycle its routes no longer wait for an approval.\n", res.out)
		require.Equal(t, []string{"approve qemu/101 addresses=[10.0.0.53]"}, e.called())
	})
	t.Run("no address", func(t *testing.T) {
		r, e := daemonWith(t, observedWaitState())

		for _, bad := range []string{"gateway", "fd00::1", "10.0.0.1/24"} {
			res := r.run("", "guest", "approve", "qemu/101", "--allow-address", bad)

			require.EqualError(t, res.err, `--allow-address "`+bad+`": want an IPv4 address`, bad)
		}
		require.Empty(t, e.called())
	})
}

func TestGuestListSaysWhyAGuestWaits(t *testing.T) {
	r, _ := daemonWith(t, observedWaitState())

	res := r.run("", "guest", "list")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "Waiting for approval (pco guest approve <owner>):\n"+
		"  qemu/101 (web-1): MAC changed from bc:24:11:00:00:01 to bc:24:11:00:00:09; "+
		"delegated: alice@pve holds VM.Config.Network; address 10.0.0.1 is the gateway of node pve1\n")
}

func TestGuestApproveRefused(t *testing.T) {
	r, e := daemonWith(t, approvalState())
	e.guestErr = fmt.Errorf("%w: qemu/109 is not in the last listing of Proxmox; approve what you can see", engine.ErrRefused)

	res := r.run("", "guest", "approve", "qemu/109")

	require.EqualError(t, res.err, "refused: qemu/109 is not in the last listing of Proxmox; approve what you can see")
	require.Empty(t, res.out)
}

func TestGuestRevokeShowsTheApprovalAndAsks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		args    []string
		revoked bool
	}{
		{"yes", "yes\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := &fakeEngine{state: approvalState(), approvals: someApprovals()}
			r := newRunner(t, serveFake(t, e))

			res := r.tty().run(tt.in, append([]string{"guest", "revoke", "qemu/101"}, tt.args...)...)

			require.Contains(t, res.out, "qemu/101 (web-1) is approved in identity uuid:101.\n"+
				"Revoking it stops its routes from being published while the admission mode is approve.\n")
			if tt.revoked {
				require.NoError(t, res.err)
				require.Equal(t, []string{"revoke qemu/101"}, e.called())
				require.Contains(t, res.out, "Revoked the approval of qemu/101.\n")
			} else {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called())
			}
			if len(tt.args) == 0 {
				require.Equal(t, "Revoke the approval of qemu/101? [y/N] ", res.errOut)
			}
		})
	}
}

func TestGuestRevokeOfAGuestWithoutApprovalSendsNothing(t *testing.T) {
	e := &fakeEngine{state: approvalState(), approvals: someApprovals()}
	r := newRunner(t, serveFake(t, e))

	res := r.tty().run("y\n", "guest", "revoke", "qemu/109")

	require.NoError(t, res.err)
	require.Equal(t, "qemu/109 has no approval; there is nothing to revoke.\n", res.out)
	require.Empty(t, res.errOut, "nothing was asked")
	require.Empty(t, e.called(), "nothing was sent")
}

func TestGuestCommandsNeedOneOwner(t *testing.T) {
	r, e := daemonWith(t, approvalState())

	for _, args := range [][]string{{"guest", "approve"}, {"guest", "revoke"}, {"guest", "approve", "qemu/1", "qemu/2"}} {
		require.Error(t, r.run("", args...).err, strings.Join(args, " "))
	}
	require.Empty(t, e.called())
}
