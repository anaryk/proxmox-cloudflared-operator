package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

func daemonWithClaims(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: healthyState(), claims: someClaims()}
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
				"Resolving hands it to qemu/102 (web-2): the public hostname moves to that guest, and qemu/101 (web-1) waits for it.\n")
			if tt.moved {
				require.NoError(t, res.err)
				require.Equal(t, []string{"resolve www.example.com qemu/102"}, e.called(), "the name in its normal form")
				require.Contains(t, res.out, "The claim on www.example.com is now held by qemu/102; the next cycle publishes it.\n")
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

// What the state has no claim of, the daemon refuses and says why: nothing is
// asked.
func TestClaimsResolveLeavesTheAnswerToTheDaemon(t *testing.T) {
	for _, tt := range []struct {
		name, host, owner string
	}{
		{"a hostname nobody holds", "nope.example.com", "qemu/102"},
		{"the holder itself", "www.example.com", "qemu/101"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithClaims(t)
			e.resolveErr = fmt.Errorf("%w: the daemon says no", engine.ErrNotFound)

			res := r.tty().run("y\n", "claims", "resolve", tt.host, tt.owner)

			require.ErrorIs(t, res.err, engine.ErrNotFound)
			require.Empty(t, res.out)
			require.Empty(t, res.errOut, "nothing was asked")
			require.Equal(t, []string{"resolve " + tt.host + " " + tt.owner}, e.called())
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

func TestGuestApprove(t *testing.T) {
	r, e := daemonWith(t, approvalState())

	res := r.runReader(unreadable{t}, "guest", "approve", "qemu/103")

	require.NoError(t, res.err)
	require.Equal(t, "Approved qemu/103 in the identity the daemon sees now; the next cycle publishes its routes.\n", res.out)
	require.Empty(t, res.errOut, "nothing is asked")
	require.Equal(t, []string{"approve qemu/103"}, e.called())
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

func TestGuestRevokeOfAGuestWithoutApprovalLeavesTheAnswerToTheDaemon(t *testing.T) {
	e := &fakeEngine{state: approvalState(), approvals: someApprovals(), guestErr: fmt.Errorf("%w: qemu/109 has no approval", engine.ErrNotFound)}
	r := newRunner(t, serveFake(t, e))

	res := r.tty().run("y\n", "guest", "revoke", "qemu/109")

	require.EqualError(t, res.err, "not found: qemu/109 has no approval")
	require.Empty(t, res.errOut, "nothing was asked")
	require.Equal(t, []string{"revoke qemu/109"}, e.called())
}

func TestGuestCommandsNeedOneOwner(t *testing.T) {
	r, e := daemonWith(t, approvalState())

	for _, args := range [][]string{{"guest", "approve"}, {"guest", "revoke"}, {"guest", "approve", "qemu/1", "qemu/2"}} {
		require.Error(t, r.run("", args...).err, strings.Join(args, " "))
	}
	require.Empty(t, e.called())
}
