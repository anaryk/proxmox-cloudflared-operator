package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func qemu(vmid int) model.GuestRef { return model.GuestRef{Kind: model.KindQEMU, VMID: vmid} }

// fourGuests is a listing with an approved guest, one whose identity changed
// since its approval, one that waits and one without the gate tag.
func fourGuests(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	changed := guest(102, "db-1", "db.example.com -> :5432", "broken.example.com -> :x")
	changed.Running = false
	other := untagged(guest(104, "", "x.example.com -> :80"))
	other.Node = "pve2"
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8081"), changed,
		guest(103, "new-1", "new.example.com -> :80"), other))
	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionApprove })
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/101", Identity: "uuid:101"}))
	require.NoError(t, e.store.SaveApproval(store.Approval{Owner: "qemu/102", Identity: "uuid:old"}))
	e.cycle()
	return e
}

func TestTheGuestsAreThoseOfTheLastListing(t *testing.T) {
	e := fourGuests(t)

	guests, err := e.eng.Guests()

	require.NoError(t, err)
	require.Equal(t, []GuestListView{
		{Ref: "qemu/101", Name: "web-1", Node: testNode, Running: true, Tagged: true, Identity: "uuid:101", Approval: "approved", Routes: 2},
		{Ref: "qemu/102", Name: "db-1", Node: testNode, Tagged: true, Identity: "uuid:102", Approval: "changed", Issues: 1},
		{Ref: "qemu/103", Name: "new-1", Node: testNode, Running: true, Tagged: true, Identity: "uuid:103", Approval: "waiting"},
		{Ref: "qemu/104", Node: "pve2", Running: true, Identity: "uuid:104", Approval: "not-needed"},
	}, guests)
}

func TestInAdmissionModeTagNoGuestNeedsAnApproval(t *testing.T) {
	e := fourGuests(t)
	e.settings(func(s *store.Settings) { s.Admission = store.AdmissionTag })
	e.cycle()

	guests, err := e.eng.Guests()

	require.NoError(t, err)
	approval := map[string]string{}
	for _, g := range guests {
		approval[g.Ref] = g.Approval
	}
	require.Equal(t, map[string]string{"qemu/101": "approved", "qemu/102": "changed", "qemu/103": "not-needed", "qemu/104": "not-needed"}, approval)
}

func TestNoGuestIsListedWhenTheLastCycleDidNotListThemAll(t *testing.T) {
	e := fourGuests(t)
	e.inv.set(incomplete("cluster status: no quorum", guest(101, "web-1", "www.example.com -> :8080")))
	e.cycle()

	guests, err := e.eng.Guests()

	require.NoError(t, err)
	require.Equal(t, []GuestListView{}, guests)
}

func TestTheAnnotationOfAGuestIsItsRouteBlock(t *testing.T) {
	e := newEnv(t)
	fenced := guest(101, "web-1")
	fenced.Description = "Web server\nowned by alice\n```cf-tunnel\nwww.example.com -> :8080\nbad.example.com -> :x\n```\nbackups: nightly"
	shorthand := guest(102, "db-1")
	shorthand.Description = "cf-tunnel: db.example.com -> :5432\nroot password in the vault\ncf-tunnel: dbadmin.example.com -> :8080"
	none := untagged(guest(103, "plain"))
	none.Description = "nothing to see"
	e.inv.set(snapshot(fenced, shorthand, none))
	e.cycle()

	got, err := e.eng.Annotation(qemu(101))
	require.NoError(t, err)
	require.Equal(t, AnnotationView{
		Ref:       "qemu/101",
		Block:     "```cf-tunnel\nwww.example.com -> :8080\nbad.example.com -> :x\n```",
		StartLine: 3,
		Issues:    []planner.Issue{{Guest: qemu(101), Line: 5, Col: 20, Msg: got.Issues[0].Msg}},
	}, got)

	got, err = e.eng.Annotation(qemu(102))
	require.NoError(t, err)
	require.Equal(t, AnnotationView{
		Ref:       "qemu/102",
		Block:     "cf-tunnel: db.example.com -> :5432\n\ncf-tunnel: dbadmin.example.com -> :8080",
		StartLine: 1,
		Issues:    []planner.Issue{},
	}, got)
	require.NotContains(t, got.Block, "password")

	got, err = e.eng.Annotation(qemu(103))
	require.NoError(t, err)
	require.Equal(t, AnnotationView{Ref: "qemu/103", Issues: []planner.Issue{}}, got)

	_, err = e.eng.Annotation(qemu(999))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTheAnnotationOfAGuestIsCut(t *testing.T) {
	e := newEnv(t)
	g := guest(101, "web-1")
	g.Description = "```cf-tunnel\n" + strings.Repeat("# a long comment line\n", 500) + "www.example.com -> :8080\n```"
	e.inv.set(snapshot(g))
	e.cycle()

	got, err := e.eng.Annotation(qemu(101))

	require.NoError(t, err)
	require.Len(t, []rune(got.Block), maxAnnotation)
	require.True(t, strings.HasPrefix(g.Description, got.Block))
}
