package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A guest re-created under its VMID waits for approval: its claim stays, and
// the identity change alone hands nothing to the clone that waits too.
func TestAGuestWaitingForApprovalKeepsItsClaim(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.settings(func(s *store.Settings) { s.Admission = "approve" })
	web := guest(101, "web-1", "www.example.com -> :8080")
	clone := guest(102, "web-2", "www.example.com -> :8080")
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:101"}))
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/102", Identity: "uuid:102"}))
	e.inv.set(snapshot(web, clone))
	e.cycle()
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())

	web.Identity = "uuid:999"
	e.inv.set(snapshot(web, clone))
	var st State
	for range 3 {
		e.clock.advance(61 * time.Second)
		st = e.cycle()
	}

	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, "qemu/101", claims["www.example.com"].Owner)
	require.Contains(t, st.Issues, planner.Issue{Guest: web.Ref, Msg: "waiting for approval"})
	require.Equal(t, RouteView{
		RouteStatus: planner.RouteStatus{
			Hostname: "www.example.com", Owner: "qemu/101", State: planner.StateHeld, Zone: "example.com",
			Reason: "named in the Notes of qemu/101 but not routed; claim kept",
		},
		Guest: &GuestView{GuestRef: web.Ref, Name: "web-1"},
	}, route(st, "www.example.com"))
	// Nothing is served: a tunnel of 503 blocks alone is not planned, so the
	// catch-all answers. The claim keeps the record.
	require.Equal(t, withSentinel(), e.rules())
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

func TestAGuestWithoutAnIdentityIsNeverApproved(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *store.Settings) { s.Admission = "approve" })
	g := guest(101, "web-1", "www.example.com -> :8080")
	g.Identity = ""
	e.inv.set(snapshot(g))

	st := e.cycle()

	require.Empty(t, st.Routes)
	require.Contains(t, st.Issues, planner.Issue{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Msg: "waiting for approval"})
}

func TestApprovalModeHoldsAnUnapprovedGuest(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.settings(func(s *store.Settings) { s.Admission = "approve" })
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	// An approval of another guest that once had this VMID does not count.
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:999"}))

	st := e.cycle()

	require.Empty(t, st.Routes)
	require.Contains(t, st.Issues, planner.Issue{Guest: ref, Msg: "waiting for approval"})
	require.Empty(t, e.writes())
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Empty(t, claims, "an unapproved guest claims nothing")

	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:101"}))
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	require.NotContains(t, st.Issues, planner.Issue{Guest: ref, Msg: "waiting for approval"})
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}
