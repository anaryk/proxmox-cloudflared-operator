package apifake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The tests of this file hold the fake to what the daemon does.

func TestTheDigestLeavesOutEveryTimeAsTheDaemonsDoes(t *testing.T) {
	e, c := load(t, "populated")
	d := serve(t, e)
	_, err := d.CheckCredential(context.Background(), "cred1", false)
	require.NoError(t, err, "the scenario's check was deep; the first shallow one is news")
	notices, _, err := d.Stream(context.Background(), "", 0)
	require.NoError(t, err)
	before := e.State().Digest

	c.add(time.Second)
	cred, err := d.CheckCredential(context.Background(), "cred1", false)
	require.NoError(t, err)
	require.Equal(t, t0.Add(time.Second), cred.Report.CheckedAt)
	require.Equal(t, before, e.State().Digest, "only checkedAt moved")
	nothing(t, notices)

	require.NoError(t, d.AcknowledgeSegment(context.Background(), "vmbr1", 20))
	require.NotEqual(t, before, e.State().Digest, "an acknowledgement is news")
	require.Equal(t, "admin", next(t, notices).Event.Kind)
	require.Equal(t, engine.NoticeState, next(t, notices).Kind)
}

// unusable is a scenario like first-run whose token check finds what it finds.
func unusable(t *testing.T, report string) *Engine {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{fileScenario, fileState, fileSettings} {
		data, err := builtin.ReadFile(scenariosDir + "/first-run/" + name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileReport), []byte(report), 0o600))
	e, err := Load(dir, func() time.Time { return t0 })
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

func TestATokenTheDaemonWouldRefuseIsRefusedWithItsReport(t *testing.T) {
	for _, c := range []struct {
		name, report, says string
	}{
		{"unusable", `{"token": {"id": "t", "status": "active"}, "accounts": [], "zones": [], "excluded": [], "leftovers": [],
			"checks": [{"capability": "dns.read", "scope": "example.com", "scopeId": "zone1", "ok": false, "detail": "grant Zone > DNS > Read on example.com"}],
			"deep": false, "usable": false, "checkedAt": "2026-10-01T12:00:00Z"}`, "the token cannot be used: "},
		{"unanswered", `{"token": {"id": "t", "status": "active"}, "accounts": [], "zones": [], "excluded": [], "leftovers": [],
			"checks": [{"capability": "token", "ok": false, "unanswered": true, "detail": "Cloudflare did not answer"}],
			"deep": false, "usable": false, "checkedAt": "2026-10-01T12:00:00Z"}`, "the token could not be checked: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := unusable(t, c.report)
			d := serve(t, e)
			res := d.call(t, http.MethodPost, "/v1/credentials", `{"label": "main", "token": "`+strings.Repeat("k", 40)+`"}`)
			require.Equal(t, http.StatusBadRequest, res.status, string(res.body))
			var body api.ErrorBody
			require.NoError(t, json.Unmarshal(res.body, &body))
			require.Equal(t, "invalid", body.Code)
			require.Contains(t, body.Error, c.says)
			var cred engine.CredentialView
			require.NoError(t, json.Unmarshal(body.Credential, &cred))
			require.True(t, cred.Checked)
			require.False(t, cred.Report.Usable)
			require.Empty(t, e.State().Credentials, "nothing is stored")
		})
	}
}

func TestARefusalCarriesTheBodyOfTheDaemonsRefusal(t *testing.T) {
	e, _ := load(t, "first-run")
	d := serve(t, e)
	status, body := d.steer(t, http.MethodPost, "/refuse", `{"method": "AddCredential", "code": "invalid", "message": "the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com",
		"credential": {"id": "", "label": "main", "kind": "scoped", "checked": true, "report": {"token": {"id": "t", "status": "active"}, "usable": false}}}`)
	require.Equal(t, http.StatusNoContent, status, string(body))
	res := d.call(t, http.MethodPost, "/v1/credentials", `{"label": "main", "token": "`+strings.Repeat("k", 40)+`"}`)
	require.Equal(t, http.StatusBadRequest, res.status)
	var answer api.ErrorBody
	require.NoError(t, json.Unmarshal(res.body, &answer))
	require.Equal(t, "the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com", answer.Error)
	require.JSONEq(t, `{"id": "", "label": "main", "kind": "scoped", "checked": true, "report": {"token": {"id": "t", "status": "active"},
		"accounts": null, "zones": null, "checks": null, "excluded": null, "deep": false, "usable": false, "leftovers": null}}`, string(answer.Credential))

	status, _ = d.steer(t, http.MethodPost, "/refuse", `{"method": "Guests", "code": "invalid", "message": "no", "credential": {"checked": true}}`)
	require.Equal(t, http.StatusBadRequest, status, "only a token is refused with a credential")
}

func TestAConfirmationWithdrawsTheLinesThatAskedForIt(t *testing.T) {
	e, _ := load(t, "frozen")
	st := e.State()
	asking := slices.DeleteFunc(slices.Clone(st.Problems), func(p string) bool { return !engine.AsksForConfirmation(p) })
	require.NotEmpty(t, asking)

	_, err := e.Apply(context.Background(), true, st.Offer)
	require.NoError(t, err)

	after := e.State().Problems
	for _, p := range asking {
		require.NotContains(t, after, p)
	}
	require.Contains(t, after, "23 changes wait for Cloudflare's rate limit", "a line that asked for nothing stays")
	require.Len(t, after, len(st.Problems)-len(asking))
}

func TestARevokedGuestWaitsForAnApprovalAgain(t *testing.T) {
	e, _ := load(t, "populated")
	ctx := context.Background()
	before := e.State()
	owned := func(st engine.State) []string {
		var out []string
		for _, r := range st.Routes {
			if r.Owner == "qemu/101" {
				out = append(out, r.Hostname)
			}
		}
		return out
	}
	hosts := owned(before)
	require.NotEmpty(t, hosts)

	require.NoError(t, e.RevokeGuest(ctx, "qemu/101"))
	st := e.State()
	require.Empty(t, owned(st), "its routes are taken out, as at the next cycle")
	i := slices.IndexFunc(st.Unapproved, func(u engine.UnapprovedGuest) bool { return u.String() == "qemu/101" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, engine.UnapprovedGuest{
		GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
		Identity:  "uuid:101", Hostnames: slices.Compact(slices.Sorted(slices.Values(hosts))), Why: []string{"admission mode approve"},
	}, st.Unapproved[i])
	guests, err := e.Guests()
	require.NoError(t, err)
	require.Equal(t, engine.ApprovalWaiting, guests[slices.IndexFunc(guests, func(g engine.GuestListView) bool { return g.Ref == "qemu/101" })].Approval)

	_, err = e.ApproveGuest(ctx, "qemu/101", "uuid:101", nil, nil)
	require.NoError(t, err)
	st = e.State()
	require.Equal(t, hosts, owned(st), "an approval puts its routes back")
	require.Equal(t, before.Unapproved, st.Unapproved)
	require.Equal(t, before.Digest, st.Digest)
}

func TestASlowStreamCollapsesAsTheDaemonsDoes(t *testing.T) {
	e, _ := load(t, "empty")
	d := serve(t, e)
	notices, _, err := d.Stream(context.Background(), "", 0)
	require.NoError(t, err)

	e.Pause()
	for i := range 300 {
		e.AddEvents(engine.Event{Level: "info", Kind: "route", Message: fmt.Sprintf("route %d", i)})
	}
	e.AddEvents(engine.Event{Level: "warn", Kind: "problem", Message: "stays"})
	e.Resume()

	var got []engine.Notice
	for len(got) == 0 || got[len(got)-1].Kind != engine.NoticeEvent || got[len(got)-1].Event.Kind != "problem" {
		got = append(got, next(t, notices))
	}
	require.Less(t, len(got), 300, "what waited collapsed")
	gaps := slices.DeleteFunc(slices.Clone(got), func(n engine.Notice) bool { return n.Kind != engine.NoticeGap })
	require.NotEmpty(t, gaps)
	counted := 0
	for _, n := range got {
		switch n.Kind {
		case engine.NoticeGap:
			counted += n.Gap.Count
		case engine.NoticeEvent:
			counted++
		}
	}
	require.Equal(t, 301, counted, "every event is told, one by one or in a gap")
}

func TestAClientTooFarBackHearsOfAGap(t *testing.T) {
	e, _ := load(t, "empty")
	d := serve(t, e)
	for i := range ringEvents + 5 {
		e.AddEvents(engine.Event{Level: "warn", Kind: "problem", Message: fmt.Sprintf("problem %d", i)})
	}
	notices, _, err := d.Stream(context.Background(), e.Boot(), 1)
	require.NoError(t, err)
	n := next(t, notices)
	require.Equal(t, engine.GapNotice{Boot: e.Boot(), From: 2, To: 5, Count: 4, Level: "warn"}, *n.Gap,
		"the events the ring no longer holds")
	require.Equal(t, uint64(6), next(t, notices).Event.Seq)
}

func TestTheTrafficNoticeHasTheRoutesTheDaemonsHas(t *testing.T) {
	e, c := load(t, "large")
	all := e.Traffic().Routes
	c.add(engine.TrafficInterval)
	e.mu.Lock()
	want, _ := engine.NoticeRoutes(all, nil)
	got := e.trafficNotice().Traffic
	e.mu.Unlock()
	require.Equal(t, want, got.Routes)
	require.Len(t, got.Routes, 100)
	require.Equal(t, len(all), got.RoutesTotal)
}

func TestAManualRouteIsCheckedAsTheDaemonChecksIt(t *testing.T) {
	e, _ := load(t, "populated")
	for _, v := range []engine.ManualRouteView{
		{ID: "Bad", Hostname: "x.example.com", Target: engine.ManualTarget{Kind: "address", Scheme: "http", Port: 80}},
		{ID: "x", Hostname: "x.example.com", Target: engine.ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "ftp", Port: 80}},
		{ID: "x", Hostname: "x.example.com", Target: engine.ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "http", Port: 80},
			Options: model.RouteOptions{AllowNode: true}},
	} {
		_, want := engine.CheckManualRoute(v)
		require.Error(t, want)
		_, got := e.CreateManualRoute(context.Background(), v)
		require.EqualError(t, got, want.Error())
	}
}

func TestThePcoBinaryDoesNotHoldTheFake(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to list the dependencies with")
	}
	out, err := exec.Command(gobin, "list", "-tags", "nomsgpack", "-deps", "../../../cmd/pco").Output()
	require.NoError(t, err)
	deps := strings.Fields(string(out))
	require.Contains(t, deps, "github.com/anaryk/proxmox-cloudflared-operator/internal/api", "the list is of pco")
	require.NotContains(t, deps, "github.com/anaryk/proxmox-cloudflared-operator/internal/api/apifake")
}

func TestRoutesComeBackInTheirOrder(t *testing.T) {
	routes := []engine.RouteView{
		{RouteStatus: planner.RouteStatus{Hostname: "b.example.com", Owner: "lxc/200"}},
		{RouteStatus: planner.RouteStatus{Hostname: "a.example.com", Owner: "manual/x"}},
		{RouteStatus: planner.RouteStatus{Hostname: "a.example.com", Owner: "qemu/101"}},
	}
	got := sortRoutes(slices.Clone(routes))
	require.Equal(t, []string{"qemu/101", "manual/x", "lxc/200"}, []string{got[0].Owner, got[1].Owner, got[2].Owner})
}

func TestADiagnosisForAnotherHolderIsRefusedAsTheDaemonRefusesIt(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	res := d.call(t, http.MethodGet, "/v1/diagnose?hostname=www.example.com&owner=qemu/101", "")
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	res = d.call(t, http.MethodGet, "/v1/diagnose?hostname=www.example.com&owner=qemu/102", "")
	require.Equal(t, http.StatusConflict, res.status)
	require.JSONEq(t, `{"error": "the holder changed: www.example.com is no longer held by qemu/102", "code": "holder_changed"}`, string(res.body))
	calls := e.Calls()
	require.JSONEq(t, `{"hostname": "www.example.com", "owner": "qemu/102"}`, string(calls[len(calls)-1].Args))

	status, _ := d.steer(t, http.MethodPost, "/refuse", `{"method": "Diagnose", "code": "holder_changed", "message": "www.example.com is no longer held by qemu/101"}`)
	require.Equal(t, http.StatusNoContent, status)
	res = d.call(t, http.MethodGet, "/v1/diagnose?hostname=www.example.com&owner=qemu/101", "")
	require.Equal(t, http.StatusConflict, res.status)
}
