package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheApexIsRejectedUntilAllowHostsNamesIt(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "example.com *.example.com www.example.com -> :8080")))
	e.enforce()

	st := e.cycle()

	require.Equal(t, planner.StateRejected, route(st, "example.com").State)
	require.Equal(t, `the apex of zone example.com is published only when allowHosts names it: add "example.com" to allowHosts`,
		route(st, "example.com").Reason)
	require.Equal(t, planner.StateRejected, route(st, "*.example.com").State)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	e.settings(func(s *store.Settings) { s.AllowHosts = []string{"example.com", "*.example.com"} })
	e.clock.advance(time.Minute)
	st = e.cycle()

	require.Equal(t, planner.StateActive, route(st, "example.com").State)
	require.Equal(t, []string{"*.example.com", "example.com", "www.example.com"}, e.recordNames())
}

func TestAGuestOverTheCapOfHostnamesPublishesNothing(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *store.Settings) { s.MaxHostnamesPerGuest = 1 })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	e.enforce()

	st := e.cycle()

	require.Equal(t, []planner.Issue{{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		Msg: "the Notes name 2 hostnames, more than maxHostnamesPerGuest allows (1); none of them is published until they name at most 1"}}, st.Issues)
	require.Empty(t, st.Routes)
	require.Empty(t, e.recordNames())
}

// Every guest with the gate tag counts, a template too: a clone of either is
// a tagged guest.
func TestTheStateCountsTheGuestsWithTheGateTag(t *testing.T) {
	e := newEnv(t)
	tmpl := guest(900, "base", "www.example.org -> :80")
	tmpl.Template = true
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), untagged(guest(102, "db", "")), tmpl))

	st := e.cycle()

	require.Equal(t, "tag", st.Admission)
	require.Equal(t, 2, st.GateTagged)

	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Equal(t, "approve", st.Admission)
}
