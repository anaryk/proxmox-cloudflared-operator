package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// segmentState has vmbr1 acknowledged with two routes on it, and VLAN 20 of
// vmbr1 not acknowledged with the route of qemu/120 held on it.
func segmentState() engine.State {
	st := healthyState()
	st.Segments = someSegments()
	st.Routes = append(st.Routes, engine.RouteView{
		RouteStatus: planner.RouteStatus{
			Hostname: "lab.example.com", Owner: "qemu/120", State: planner.StateUnreachable, Level: "observed",
			Reason: engine.SegmentReason("vmbr1", 20),
		},
		Guest: qemu(120, "lab"),
	})
	return st
}

func someSegments() []engine.SegmentView {
	return []engine.SegmentView{
		{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: t0.Add(-time.Hour), Routes: 2},
		{Bridge: "vmbr1", VLAN: 20, Routes: 1},
	}
}

func daemonWithSegments(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: segmentState(), segments: someSegments()}
	return newRunner(t, serveFake(t, e)), e
}

func TestSegmentList(t *testing.T) {
	r, e := daemonWithSegments(t)

	res := r.run("", "segment", "list")

	require.NoError(t, res.err)
	require.Equal(t, "SEGMENT   ACKNOWLEDGED               ROUTES\n"+
		"vmbr1     2026-10-01T13:00:00+02:00  2\n"+
		"vmbr1:20  no                         1\n", res.out)
	require.Empty(t, e.called())

	t.Run("none", func(t *testing.T) {
		r, _ := daemonWith(t, healthyState())

		res := r.run("", "segment", "list")

		require.NoError(t, res.err)
		require.Equal(t, "No route at observed was proven on a segment, and none is acknowledged.\n", res.out)
	})
}

func TestSegmentListJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"routes":1,"bridge":"vmbr1","acknowledged":false}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/segments": {200, raw}}))

	res := r.run("", "--json", "segment", "list")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

func TestSegmentAcknowledgeShowsWhatItReleasesAndAsks(t *testing.T) {
	const shown = "1 route at observed waits for segment vmbr1 VLAN 20:\n" +
		"  lab.example.com  qemu/120 (lab)\n" +
		"Acknowledging it serves the routes at observed proven on it, unless they wait for an approval of their guest.\n"
	for _, tt := range []struct {
		name  string
		in    string
		args  []string
		acked bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWithSegments(t)

			res := r.tty().run(tt.in, append([]string{"segment", "acknowledge", "vmbr1:20"}, tt.args...)...)

			require.Contains(t, res.out, shown)
			if !tt.acked {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called())
				return
			}
			require.NoError(t, res.err)
			require.Equal(t, []string{"acknowledge vmbr1:20"}, e.called())
			require.Equal(t, shown+"Acknowledged segment vmbr1 VLAN 20; its routes at observed are served from the next cycle.\n", res.out)
			if len(tt.args) == 0 {
				require.Equal(t, "Acknowledge segment vmbr1 VLAN 20? [y/N] ", res.errOut)
			}
		})
	}
}

func TestSegmentAcknowledgeOfASegmentNothingWaitsFor(t *testing.T) {
	r, e := daemonWithSegments(t)

	res := r.run("", "segment", "acknowledge", "vmbr2", "--yes")

	require.NoError(t, res.err)
	require.Equal(t, "No route waits for segment vmbr2 now.\n"+
		"Acknowledging it serves the routes at observed proven on it, unless they wait for an approval of their guest.\n"+
		"Acknowledged segment vmbr2; its routes at observed are served from the next cycle.\n", res.out)
	require.Equal(t, []string{"acknowledge vmbr2:0"}, e.called())
}

func TestSegmentAcknowledgeOfOneAcknowledgedAlready(t *testing.T) {
	r, e := daemonWithSegments(t)

	res := r.tty().run("y\n", "segment", "acknowledge", "vmbr1")

	require.NoError(t, res.err)
	require.Equal(t, "Segment vmbr1 is acknowledged since 2026-10-01T13:00:00+02:00; there is nothing to do.\n", res.out)
	require.Empty(t, res.errOut, "nothing is asked")
	require.Empty(t, e.called())
}

func TestSegmentRevokeShowsWhatItHoldsAndAsks(t *testing.T) {
	r, e := daemonWithSegments(t)

	res := r.tty().run("yes\n", "segment", "revoke", "vmbr1")

	require.NoError(t, res.err)
	require.Equal(t, "Segment vmbr1 is acknowledged since 2026-10-01T13:00:00+02:00; 2 routes at observed were proven on it in the last cycle.\n"+
		"Revoking it holds the routes at observed on it from the next cycle, until it is acknowledged again.\n"+
		"Revoked the acknowledgement of segment vmbr1.\n", res.out)
	require.Equal(t, "Revoke the acknowledgement of segment vmbr1? [y/N] ", res.errOut)
	require.Equal(t, []string{"revoke segment vmbr1:0"}, e.called())

	t.Run("one not acknowledged", func(t *testing.T) {
		r, e := daemonWithSegments(t)

		res := r.tty().run("y\n", "segment", "revoke", "vmbr1:20")

		require.NoError(t, res.err)
		require.Equal(t, "Segment vmbr1 VLAN 20 is not acknowledged; there is nothing to revoke.\n", res.out)
		require.Empty(t, e.called())
	})
}

func TestASegmentIsABridgeAndAVLAN(t *testing.T) {
	r, e := daemonWithSegments(t)
	for _, arg := range []string{"", ":20", "vmbr1:", "vmbr1:x", "vmbr1:-1", "vmbr1:20:3", "vmbr1:4095"} {
		res := r.run("", "segment", "acknowledge", arg, "--yes")

		require.EqualError(t, res.err, `segment "`+arg+`": want <bridge> or <bridge>:<vlan>, as vmbr1 or vmbr1:20, with a VLAN of 1 to 4094`, arg)
	}
	require.Empty(t, e.called())
}

func TestStatusSaysWhenASegmentIsNotAcknowledged(t *testing.T) {
	r, _ := daemonWith(t, segmentState())

	res := r.run("", "status")

	require.Contains(t, res.out, "Routes:      active 3, unreachable 2\nSegments:    1 not acknowledged (pco segment list)\n")

	st := healthyState()
	st.Segments = someSegments()[:1]
	r, _ = daemonWith(t, st)
	require.NotContains(t, r.run("", "status").out, "Segments:")
}
