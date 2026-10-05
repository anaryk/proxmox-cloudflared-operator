package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var refWeb = model.GuestRef{Kind: model.KindQEMU, VMID: 101}

// approving is an engine in enforce mode with admission approve whose last
// cycle saw qemu/101 wait for approval.
func approving(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
	st := e.cycle()
	require.Empty(t, st.Routes)
	require.Equal(t, []UnapprovedGuest{{
		GuestView: GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:101", Hostnames: []string{"www.example.com"},
	}}, st.Unapproved)
	return e
}

func approveWeb(t *testing.T, e *env) Approval {
	t.Helper()
	a, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "")
	require.NoError(t, err)
	return a
}

func TestAnOwnerIsNamedWithItsGuest(t *testing.T) {
	require.Equal(t, "qemu/101 (web-1)", OwnerName("qemu/101", &GuestView{GuestRef: refWeb, Name: "web-1"}))
	require.Equal(t, "qemu/101", OwnerName("qemu/101", &GuestView{GuestRef: refWeb}))
	require.Equal(t, "manual/www", OwnerName("manual/www", nil))
}

// approvals returns the identity approved for each owner.
func approvals(t *testing.T, e *env) map[string]string {
	t.Helper()
	a, err := e.store.Approvals()
	require.NoError(t, err)
	out := make(map[string]string, len(a))
	for owner, approval := range a {
		out[owner] = approval.Identity
	}
	return out
}

func TestApprovingAGuestPublishesIt(t *testing.T) {
	e := approving(t)
	since := e.clock.now()
	drain(e)

	a, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101")

	require.NoError(t, err)
	require.Equal(t, Approval{Owner: "qemu/101", Guest: &GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:101", Mode: "approve"}, a)
	requireTriggered(t, e)
	require.Equal(t, map[string]string{"qemu/101": "uuid:101"}, approvals(t, e), "the identity the listing showed")
	require.Equal(t, []string{"qemu/101: qemu/101 (web-1) is approved in identity uuid:101"}, adminEvents(e, since.Add(-time.Nanosecond)))
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	require.Empty(t, st.Unapproved)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

// The approval is of the guest as it was seen: a guest re-created under the
// same VMID, or a clone, is a guest nobody approved.
func TestAnApprovalIsOfOneIdentity(t *testing.T) {
	t.Run("a re-created guest waits again", func(t *testing.T) {
		e := approving(t)
		approveWeb(t, e)
		e.clock.advance(10 * time.Second)
		require.Equal(t, planner.StateActive, route(e.cycle(), "www.example.com").State)

		again := guest(101, "web-1", "www.example.com -> :8080")
		again.Identity = "uuid:999"
		e.inv.set(snapshot(again))
		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Equal(t, planner.StateHeld, route(st, "www.example.com").State)
		require.Contains(t, st.Issues, planner.Issue{Guest: refWeb, Msg: issueWaitingApproval})
		require.Equal(t, []UnapprovedGuest{{
			GuestView: GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:999", Hostnames: []string{"www.example.com"},
		}}, st.Unapproved)
		require.Equal(t, withSentinel(), e.rules())

		approveWeb(t, e)
		require.Equal(t, "uuid:999", approvals(t, e)["qemu/101"])
		e.clock.advance(10 * time.Second)
		require.Equal(t, planner.StateActive, route(e.cycle(), "www.example.com").State)
	})
	t.Run("a clone waits", func(t *testing.T) {
		e := approving(t)
		approveWeb(t, e)
		clone := guest(102, "web-1", "api.example.com -> :8080")
		clone.Identity = "uuid:101"
		e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), clone))
		e.clock.advance(10 * time.Second)

		st := e.cycle()

		require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
		require.Empty(t, route(st, "api.example.com").State, "the clone serves nothing")
		require.Equal(t, []UnapprovedGuest{{
			GuestView: GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, Name: "web-1"},
			Identity:  "uuid:101", Hostnames: []string{"api.example.com"},
		}}, st.Unapproved)
	})
}

// An approval names the identity the admin was shown: a guest re-created
// since is not approved by it.
func TestAnApprovalOfAnotherIdentityThanTheGuestHasIsRefused(t *testing.T) {
	e := approving(t)
	again := guest(101, "web-1", "www.example.com -> :8080")
	again.Identity = "uuid:999"
	e.inv.set(snapshot(again))
	e.clock.advance(10 * time.Second)
	e.cycle()
	since := e.clock.now()

	_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "uuid:101")

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: qemu/101 changed since it was shown: it was shown in identity uuid:101 "+
		"and has identity uuid:999 now; look at it again")
	require.Empty(t, approvals(t, e))
	require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
}

// The guests that wait are listed in the natural order of their owners.
func TestTheGuestsThatWaitAreInOrder(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080"), guest(20, "old", "old.example.com -> :8080")))

	st := e.cycle()

	require.Equal(t, []UnapprovedGuest{
		{GuestView: GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 20}, Name: "old"}, Identity: "uuid:20", Hostnames: []string{"old.example.com"}},
		{GuestView: GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:101", Hostnames: []string{"api.example.com", "www.example.com"}},
	}, st.Unapproved)
}

// Only what a complete listing showed is approved.
func TestApprovingIsRefusedWithoutACompleteListing(t *testing.T) {
	const notAll = "refused: the last cycle did not list every guest, so qemu/101 cannot be approved as it is now; " +
		"approve what you can see once a cycle has listed them all"
	for _, tt := range []struct {
		name  string
		setup func(e *env)
		want  string
	}{
		{"before the first cycle", func(*env) {}, notAll},
		{"after a cycle with an incomplete inventory", func(e *env) {
			e.cycle()
			e.inv.set(incomplete("cluster status: no quorum", guest(101, "web-1", "www.example.com -> :8080")))
			e.clock.advance(10 * time.Second)
			e.cycle()
		}, notAll},
		{"a guest the listing does not have", func(e *env) {
			e.inv.set(snapshot(guest(102, "other", "api.example.com -> :8080")))
			e.cycle()
		}, "refused: qemu/101 is not in the last listing of Proxmox; approve what you can see"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
			tt.setup(e)
			since := e.clock.now()

			_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "")

			require.ErrorIs(t, err, ErrRefused)
			require.EqualError(t, err, tt.want)
			require.Empty(t, approvals(t, e))
			require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
		})
	}
}

func TestAGuestWithoutAnIdentityCannotBeApproved(t *testing.T) {
	e := newEnv(t)
	g := guest(101, "web-1", "www.example.com -> :8080")
	g.Identity = ""
	e.inv.set(snapshot(g))
	e.cycle()

	_, err := e.eng.ApproveGuest(t.Context(), "qemu/101", "")

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, "refused: qemu/101 has no identity Proxmox reports, and an approval is of one")
	require.Empty(t, approvals(t, e))
}

func TestOnlyAGuestIsApproved(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	for _, owner := range []string{"manual/www", "web-1", "", "qemu/0101"} {
		_, err := e.eng.ApproveGuest(t.Context(), owner, "")
		require.ErrorIs(t, err, ErrInvalid, owner)
		require.ErrorIs(t, e.eng.RevokeGuest(t.Context(), owner), ErrInvalid, owner)
	}
	require.Empty(t, approvals(t, e))
}

func TestRevokingAnApprovalHoldsTheGuestAgain(t *testing.T) {
	e := approving(t)
	approveWeb(t, e)
	e.clock.advance(10 * time.Second)
	require.Equal(t, planner.StateActive, route(e.cycle(), "www.example.com").State)
	since := e.clock.now()

	require.NoError(t, e.eng.RevokeGuest(t.Context(), "qemu/101"))

	require.Empty(t, approvals(t, e))
	require.Equal(t, []string{"qemu/101: the approval of qemu/101 (web-1) is revoked"}, adminEvents(e, since.Add(-time.Nanosecond)))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, planner.StateHeld, route(st, "www.example.com").State, "it keeps its claim and serves nothing")
	require.Equal(t, withSentinel(), e.rules())

	err := e.eng.RevokeGuest(t.Context(), "qemu/101")
	require.ErrorIs(t, err, ErrNotFound)
	require.EqualError(t, err, "not found: qemu/101 has no approval")
}

// In admission mode tag an approval is recorded all the same, and the event
// says that it does not matter yet.
func TestApprovalsInTagModeAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	since := e.clock.now()

	a := approveWeb(t, e)
	require.Equal(t, "tag", a.Mode)
	require.NoError(t, e.eng.RevokeGuest(t.Context(), "qemu/101"))

	require.Equal(t, []string{
		"qemu/101: qemu/101 (web-1) is approved in identity uuid:101; the admission mode is tag, so it matters only once the mode is approve",
		"qemu/101: the approval of qemu/101 (web-1) is revoked; the admission mode is tag, so it matters only once the mode is approve",
	}, adminEvents(e, since.Add(-time.Nanosecond)))
	st := e.cycle()
	require.Empty(t, st.Unapproved, "in mode tag nobody waits for approval")
}

func TestApprovalsAreListedWithTheIdentityTheGuestHasNow(t *testing.T) {
	e := approving(t)
	approveWeb(t, e)
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/102", Identity: "uuid:old"}))
	require.NoError(t, e.store.SaveApproval(store.Approval{
		Owner: "lxc/300", Identity: "uuid:300", MACs: []string{"bc:24:11:00:03:00"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
	}))
	moved := guest(102, "db-1", "db.example.com -> :5432")
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), moved))
	e.clock.advance(10 * time.Second)
	e.cycle()

	views, err := e.eng.Approvals()

	require.NoError(t, err)
	require.Equal(t, []ApprovalView{
		{Owner: "qemu/101", Guest: &GuestView{GuestRef: refWeb, Name: "web-1"}, Identity: "uuid:101", Current: "uuid:101", Matches: true},
		{Owner: "qemu/102", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, Name: "db-1"}, Identity: "uuid:old", Current: "uuid:102"},
		{
			Owner: "lxc/300", Guest: &GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 300}}, Identity: "uuid:300",
			MACs: []string{"bc:24:11:00:03:00"}, Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
		},
	}, views)
}
