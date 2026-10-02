package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestNewRefusesMissingDependencies(t *testing.T) {
	e := newEnv(t)
	full := Deps{
		Store: e.store, Inventory: e.inv, Resolver: e.res, Connectors: e.conn,
		NewClient: e.newClient, Node: testNode, Log: zerolog.Nop(),
	}
	for name, change := range map[string]func(*Deps){
		"store":      func(d *Deps) { d.Store = nil },
		"inventory":  func(d *Deps) { d.Inventory = nil },
		"resolver":   func(d *Deps) { d.Resolver = nil },
		"connectors": func(d *Deps) { d.Connectors = nil },
		"client":     func(d *Deps) { d.NewClient = nil },
		"node":       func(d *Deps) { d.Node = "" },
	} {
		d := full
		change(&d)
		_, err := New(d)
		require.Error(t, err, name)
	}
	_, err := New(full)
	require.NoError(t, err)
}

func TestFirstCycleObservesAndWritesNothing(t *testing.T) {
	e := newEnv(t)

	st := e.cycle()

	require.Equal(t, "observe", st.Mode)
	require.True(t, st.Complete)
	require.Empty(t, st.Problems)
	require.Equal(t, "ok", st.WriterVerdict)
	require.Empty(t, e.writes(), "observe mode changes nothing at Cloudflare")
	require.Equal(t, []string{
		"create-tunnel pco-abc123 held: observe mode",
		"put-config pco-abc123 held: observe mode",
		"create-record www.example.com held: tunnel not created yet",
	}, actionKinds(st))
	require.Equal(t, RouteView{
		RouteStatus: planner.RouteStatus{
			Hostname: "www.example.com", Owner: "qemu/101", State: planner.StateActive,
			Service: "http://10.0.0.11:8080", Zone: "example.com",
		},
		Guest:      &GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
		Candidates: []resolve.CandidateResult{{Addr: guestAddr, Source: resolve.FromStatic, OK: true}},
	}, route(st, "www.example.com"))
	require.Equal(t, []TunnelView{{TunnelState: reconcile.TunnelState{AccountID: testAccount, CredentialID: testCred, Name: tunnelName}}}, st.Tunnels)
	require.Empty(t, e.conn.ensures(), "observe mode starts no connector")
	require.Empty(t, e.conn.prunes())

	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, "qemu/101", claims["www.example.com"].Owner, "claims are pco's own state and kept in observe mode too")
}

func TestApplyPublishesTunnelConfigAndRecord(t *testing.T) {
	e := newEnv(t)
	e.cycle()

	e.apply(false)
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Equal(t, "enforce", st.Mode)
	require.Empty(t, st.Problems)
	require.Len(t, e.tunnels(), 1)
	tun := e.tunnels()[0]
	require.Equal(t, tunnelName, tun.Name)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
	recs := e.records()
	require.Len(t, recs, 1)
	require.Equal(t, "www.example.com", recs[0].Name)
	require.Equal(t, "CNAME", recs[0].Type)
	require.Equal(t, tun.ID+".cfargotunnel.com", recs[0].Content)
	require.True(t, recs[0].Proxied)
	require.Equal(t, marker, recs[0].Comment)
	require.Equal(t, []string{
		"create-tunnel pco-abc123",
		"put-config pco-abc123",
		"create-record www.example.com",
	}, actionKinds(st))

	require.Equal(t, []ensureCall{{id: tun.ID, token: cffake.RunToken(testAccount, tun.ID)}}, e.conn.ensures())
	require.Equal(t, [][]string{{tun.ID}}, e.conn.prunes())
	var applied []string
	for _, ev := range e.eng.Events(t0) {
		if ev.Kind == "action" {
			applied = append(applied, ev.Subject+": "+ev.Message)
		}
	}
	require.Equal(t, []string{
		"pco-abc123: create-tunnel in account acc1",
		"pco-abc123: put-config in account acc1: 3 rules, first configuration",
		"www.example.com: create-record in zone example.com: CNAME " + tun.ID + ".cfargotunnel.com",
	}, applied)
	require.Equal(t, []connector.Status{{TunnelID: tun.ID, Active: true, Ready: true, Connections: 4, MetricsAddr: "127.0.0.1:20300"}}, st.Connectors)
	require.Len(t, st.Tunnels, 1)
	require.True(t, st.Tunnels[0].Exists)
	require.True(t, st.Tunnels[0].Verified)

	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, guestAddr, bindings["www.example.com"].Addr)
}

// Apply waits for the running cycle and confirms what its state shows.
func TestApplyWaitsForTheRunningCycleAndKeepsItsConfirmation(t *testing.T) {
	e := guarded(t, nil)
	// A route of another guest, so that the cycle reaches the resolver.
	e.inv.set(snapshot(untagged(guest(101, "web-1")), guest(102, "keep", "keep.example.com -> :8080")))
	e.clock.advance(10 * time.Second)
	inCycle := make(chan struct{})
	release := make(chan struct{})
	e.res.hook(func() {
		close(inCycle)
		<-release
	})
	done := make(chan State)
	go func() { done <- e.eng.Cycle(t.Context()) }()
	<-inCycle

	applied := make(chan error)
	go func() { applied <- e.eng.Apply(t.Context(), true) }()
	e.res.hook(nil)
	close(release)
	require.Equal(t, "enforce", (<-done).Mode)
	require.NoError(t, <-applied)

	require.NotNil(t, e.eng.confirm, "the cycle that ran showed the guard holding")
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Nil(t, e.eng.confirm, "the next DNS run used it")
	require.Equal(t, []string{"keep.example.com"}, e.recordNames(), "the six went")
}

func TestRouteRemovedRuleGoesAtOnceRecordAfterTheGrace(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	e.inv.set(snapshot(untagged(guest(101, "web-1", "www.example.com -> :8080"))))
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, withSentinel(), e.rules(), "the rule is gone at once")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Empty(t, bindings, "a hostname nobody serves keeps no binding")
	require.Equal(t, []string{"www.example.com"}, e.recordNames(), "the claim keeps the record through its grace")
	require.Equal(t, planner.StateHeld, route(st, "www.example.com").State)

	// The claim is released after its grace; the record has a grace of its own.
	e.clock.advance(61 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Routes)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	require.Contains(t, actionKinds(st), "delete-record www.example.com held: grace period: 1m0s left")

	e.clock.advance(61 * time.Second)
	st = e.cycle()

	require.Empty(t, e.records(), "deleted after the grace")
	require.Contains(t, actionKinds(st), "delete-record www.example.com")
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Empty(t, claims)
}

// D2: the planner gives a route whose target was rejected no record, and its
// record is retired rather than kept for the claim.
func TestTheRecordOfARejectedTargetIsRetired(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	e.res.reject("www.example.com", "address of a cluster node")
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, actionKinds(st), "delete-record www.example.com held: grace period: 1m0s left")
	e.clock.advance(61 * time.Second)
	e.cycle()
	require.Empty(t, e.records())
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Contains(t, claims, "www.example.com", "the claim stays with its owner")
}

// rejectedAndDue is an engine whose www.example.com was rejected long enough
// for its record to be due.
func rejectedAndDue(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.cycle()
	e.res.reject("www.example.com", "address of a cluster node")
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	return e
}

// Item 2: the guest changed its route between the cycle and the look right
// before the delete; the changed route wants its record.
func TestTheSecondLookCountsAChangedRouteAsWanted(t *testing.T) {
	e := rejectedAndDue(t)
	web := guest(101, "web-1", "www.example.com -> :8080")
	e.inv.enqueue(snapshot(web), snapshot(guest(101, "web-1", "www.example.com -> :9090")))

	e.cycle()

	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

// Item 7: another owner's route for the name wants its record.
func TestTheSecondLookCountsAnotherOwnersRouteAsWanted(t *testing.T) {
	e := rejectedAndDue(t)
	web := guest(101, "web-1", "www.example.com -> :8080")
	e.inv.enqueue(snapshot(web), snapshot(web, guest(102, "web-2", "www.example.com -> :8080")))

	e.cycle()

	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

// Review Focus 1: the daemon was killed after it created the tunnel and
// before it stored anything.
func TestCycleRecoversTunnelByName(t *testing.T) {
	t.Run("no token on disk", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		tun := e.cf.SeedTunnel(testAccount, tunnelName, nil)

		st := e.cycle()

		require.Empty(t, st.Problems)
		require.Equal(t, []cfapi.Tunnel{tun}, e.tunnels(), "no second tunnel")
		require.NotContains(t, e.writes(), "CreateTunnel "+testAccount+" "+tunnelName)
		require.Contains(t, e.cf.Calls(), "TunnelToken "+testAccount+" "+tun.ID)
		require.Equal(t, []ensureCall{{id: tun.ID, token: cffake.RunToken(testAccount, tun.ID)}}, e.conn.ensures())
		require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
		require.Equal(t, []string{"www.example.com"}, e.recordNames())
	})
	t.Run("token on disk", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		tun := e.cf.SeedTunnel(testAccount, tunnelName, nil)
		e.conn.tokens[tun.ID] = "token-on-disk"

		e.cycle()

		require.NotContains(t, e.cf.Calls(), "TunnelToken "+testAccount+" "+tun.ID)
		require.Equal(t, []ensureCall{{id: tun.ID, token: "token-on-disk"}}, e.conn.ensures(), "a stopped unit is started with its own token")
	})
	t.Run("a fresh engine after a crash", func(t *testing.T) {
		e := newEnv(t)
		e.enforce()
		e.cycle()
		first := e.tunnels()
		require.Len(t, first, 1)

		e.eng = e.newEngine()
		e.clock.advance(10 * time.Second)
		st := e.cycle()

		require.Empty(t, st.Problems)
		require.Equal(t, first, e.tunnels())
		require.Empty(t, actionKinds(st), "nothing left to do")
	})
}

func TestUnregisteredNodeRefusesToRun(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.store.DeleteNode(testNode))
		require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: "pve2", Since: t0}))

		st := e.cycle()

		require.Equal(t, []string{"node pve1 is not registered; run pco setup"}, st.Problems)
		require.Zero(t, e.inv.refreshes())
		require.Empty(t, e.cf.Calls())
		require.Empty(t, e.conn.ensures())
		require.Empty(t, e.conn.prunes())
	})
	t.Run("another node besides", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: "pve2", Since: t0}))

		st := e.cycle()

		require.Equal(t, []string{"the node registry names pve2 besides pve1; pco runs on one registered node only"}, st.Problems)
		require.Zero(t, e.inv.refreshes())
		require.Empty(t, e.cf.Calls())
	})
}

func TestEventsForARouteGoingActiveThenUnreachable(t *testing.T) {
	e := newEnv(t)
	e.cycle()

	require.Contains(t, unnumbered(e.eng.Events(time.Time{})), Event{
		At: t0, Level: "info", Kind: "route", Subject: "www.example.com", Message: "qemu/101: active",
	})

	e.res.setUnreachable("www.example.com", "connection refused")
	e.clock.advance(10 * time.Second)
	e.cycle()

	later := e.eng.Events(t0)
	require.Equal(t, []Event{{
		At: t0.Add(10 * time.Second), Level: "warn", Kind: "route", Subject: "www.example.com",
		Message: "qemu/101: unreachable (connection refused)",
	}}, unnumbered(later), "only what changed, and nothing from before since")

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, e.eng.Events(t0.Add(10*time.Second)), "an unchanged world has no events")

	logged := readEventLog(t, e)
	require.Contains(t, logged, later[0])
}

func readEventLog(t *testing.T, e *env) []Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.paths.Local, eventsFile))
	require.NoError(t, err)
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev Event
		require.NoError(t, json.Unmarshal([]byte(line), &ev))
		out = append(out, ev)
	}
	return out
}

func TestAddCredentialWithABadTokenStoresNothing(t *testing.T) {
	e := newEnv(t)
	bad := cffake.New()
	bad.SetTokenStatus("disabled", nil)
	e.useAPI("bad-token", bad)

	view, err := e.eng.AddCredential(t.Context(), "second", "bad-token")

	require.ErrorIs(t, err, ErrInvalid)
	require.Contains(t, err.Error(), "token is disabled")
	require.Empty(t, view.ID)
	require.False(t, view.Report.Usable)
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1, "only the credential that was there")
	require.Empty(t, bad.Calls()[1:], "the shallow check stops at the token")
}

func TestAddCredentialStoresAUsableToken(t *testing.T) {
	e := newEnv(t)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	e.useAPI("good-token", other)

	view, err := e.eng.AddCredential(t.Context(), " second ", "good-token\n")

	require.NoError(t, err)
	require.Regexp(t, `^[0-9a-f]{8}$`, view.ID)
	require.Equal(t, "second", view.Label)
	require.Equal(t, "scoped", view.Kind)
	require.True(t, view.Report.Usable)
	for _, c := range other.Calls() {
		require.NotRegexp(t, `^(Create|Update|Delete|Put)`, c, "a shallow check changes nothing")
	}
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 2)

	st := e.cycle()
	i := slices.IndexFunc(st.Credentials, func(c CredentialView) bool { return c.ID == view.ID })
	require.GreaterOrEqual(t, i, 0)
	require.True(t, st.Credentials[i].Report.Usable, "the state shows the report of the check")
}

func TestAddCredentialRefusesEmptyInput(t *testing.T) {
	e := newEnv(t)
	_, err := e.eng.AddCredential(t.Context(), "", "x")
	require.ErrorIs(t, err, ErrInvalid)
	_, err = e.eng.AddCredential(t.Context(), "l", " ")
	require.ErrorIs(t, err, ErrInvalid)
}

func TestCheckCredential(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.CheckCredential(t.Context(), "nope", false)
	require.ErrorIs(t, err, ErrNotFound)

	view, err := e.eng.CheckCredential(t.Context(), testCred, true)
	require.NoError(t, err)
	require.Equal(t, testCred, view.ID)
	require.True(t, view.Report.Deep)
	require.True(t, view.Report.Usable)
	require.Empty(t, e.records(), "the probes are gone again")

	st := e.cycle()
	require.Equal(t, view.Report, st.Credentials[0].Report)
}

func TestRemoveCredential(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()

	err := e.eng.RemoveCredential(t.Context(), testCred)

	require.ErrorIs(t, err, ErrRefused)
	require.Contains(t, err.Error(), "record www.example.com in zone example.com")
	require.Contains(t, err.Error(), "tunnel pco-abc123 in account acc1")
	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)

	require.ErrorIs(t, e.eng.RemoveCredential(t.Context(), "nope"), ErrNotFound)

	e.cf.FailNext("zones", 1, errors.New("connection reset"))
	err = e.eng.RemoveCredential(t.Context(), testCred)
	require.ErrorIs(t, err, ErrRefused, "what cannot be seen is not taken for nothing")
	require.Contains(t, err.Error(), "connection reset")
}

func TestACheckedCredentialSaysSo(t *testing.T) {
	e := newEnv(t)
	st := e.cycle()
	require.False(t, st.Credentials[0].Checked, "never checked in this process")

	_, err := e.eng.CheckCredential(t.Context(), testCred, false)
	require.NoError(t, err)
	st = e.cycle()
	require.True(t, st.Credentials[0].Checked)
}

func TestCheckingACredentialThatIsRemovedMeanwhile(t *testing.T) {
	e := newEnv(t)
	var once sync.Once
	e.addSecondCredential("second-token", hookedAPI{API: e.cf, before: func(method string) {
		if method == "Records" {
			once.Do(func() { require.NoError(t, e.store.DeleteCredential("cred2")) })
		}
	}})

	_, err := e.eng.CheckCredential(t.Context(), "cred2", false)

	require.ErrorIs(t, err, ErrNotFound)
	st := e.cycle()
	require.Len(t, st.Credentials, 1)
	require.NotContains(t, e.eng.reports, "cred2", "a removed credential gets no report back")
}

func TestRemovingACredentialWhoseTokenIsRefused(t *testing.T) {
	e := newEnv(t)
	refused := cffake.New()
	refused.Deny("zones")
	e.addSecondCredential("refused-token", refused)

	require.NoError(t, e.eng.RemoveCredential(t.Context(), "cred2"))

	require.Contains(t, adminEvents(e, time.Time{}), `cred2: credential "label-cred2" removed; Cloudflare refused its token, `+
		"so what it managed could not be checked and may be left behind")
}

func TestRemoveCredentialThatManagesNothing(t *testing.T) {
	e := newEnv(t)
	idle := cffake.New()
	idle.AddAccount("acc2", "Other")
	idle.AddZone("zone2", "example.org", "acc2")
	idle.SeedRecord("zone2", cfapi.Record{Type: "TXT", Name: "_pco-probe-x.example.org", Content: "p", Comment: marker + " probe"})
	e.useAPI("idle-token", idle)
	require.NoError(t, e.store.SaveCredential(store.Credential{ID: "cred2", Label: "idle", Kind: "scoped", Token: store.NewSecret("idle-token"), AddedAt: t0}))
	e.cycle()

	require.NoError(t, e.eng.RemoveCredential(t.Context(), "cred2"))

	creds, err := e.store.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
	st := e.cycle()
	require.Len(t, st.Credentials, 1)
	require.NotContains(t, e.eng.clients, "cred2")
}

func TestAdoptRefusesAnInvalidName(t *testing.T) {
	e := newEnv(t)
	require.ErrorIs(t, e.eng.Adopt(t.Context(), "not a name"), ErrInvalid)
}
