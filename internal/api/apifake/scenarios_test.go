package apifake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var update = flag.Bool("update", false, "write the scenarios anew from the goldens")

// The goldens the scenarios are made of.
const (
	engineGoldens = "../../engine/testdata"
	apiGoldens    = "../testdata"
)

// madeScenarios are the scenarios of the package as the goldens make them,
// by name. The words the daemon would say of what a scenario adds are the
// daemon's own, written out where they are not exported.
func madeScenarios(t *testing.T) map[string]files {
	populated := populatedScenario(t)
	return map[string]files{
		"populated":    populated,
		"empty":        emptyScenario(t),
		"first-run":    firstRunScenario(t),
		"hold":         holdScenario(populated),
		"frozen":       frozenScenario(populated),
		"egress-off":   egressOffScenario(populated),
		"rogue":        rogueScenario(t, populated),
		"untagged":     untaggedScenario(t, populated, 0),
		"tagged-empty": untaggedScenario(t, populated, 3),
		"reader": {About: "populated, for a reader: the web interface filters it by the guests the reader may audit",
			Like: "populated"},
		"large": {About: "1000 routes over 6 zones, 40 of them not active, 2000 guests, a figure of traffic for every route served",
			Generate: "large"},
		"outage": {About: "1000 routes, 600 of them unreachable for one of three reasons", Generate: "outage"},
		"wide":   {About: "50 zones in 30 accounts, so 30 tunnels, and 300 routes", Generate: "wide"},
	}
}

func golden[T any](t *testing.T, dir, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	var v T
	require.NoError(t, strict(data, &v), name)
	return v
}

// populatedScenario is the populated state of the engine with the examples of
// the mockup: a route in every state, two routes on one target, a manual
// route, a zone left out and one frozen, traffic on the trunk and to every
// target. Its cycle checked Cloudflare, and the egress filter is on.
func populatedScenario(t *testing.T) files {
	st := golden[engine.State](t, engineGoldens, "state_populated.json")
	st.Hold, st.Egress = "", engine.EgressView{State: engine.EgressOn}
	for i := range st.Tunnels {
		if tv := &st.Tunnels[i]; tv.Unchecked {
			tv.Unchecked, tv.Held, tv.Verified = false, "", true
		}
	}
	www := st.Routes[0]
	frozenWhy := st.Zones[slices.IndexFunc(st.Zones, func(z engine.ZoneView) bool { return z.State == engine.ZoneFrozen })].FrozenWhy
	guest := func(kind model.GuestKind, vmid int) model.GuestRef { return model.GuestRef{Kind: kind, VMID: vmid} }

	app := clone(www)
	app.Hostname, app.Service, app.Warnings = "app.example.com", "http://10.0.0.11:3000", nil
	app.Rule = &planner.IngressRule{Hostname: app.Hostname, Service: app.Service}
	git := served(www, "git.example.com", guest(model.KindQEMU, 110), "gitea", "https://10.0.0.30:3000", "tap110i0")
	git.Candidates[0].Source = resolve.FromAgent
	git.Rule.OriginServerName = "git.example.com"
	held := engine.RouteView{
		RouteStatus: planner.RouteStatus{Hostname: "old.example.com", Owner: "qemu/101", State: planner.StateHeld, Zone: "example.com",
			Reason: "no longer claimed by qemu/101 since 2026-10-01T12:01:00Z; released after the grace period"},
		Guest: www.Guest, Account: www.Account, Rule: &planner.IngressRule{Hostname: "old.example.com", Service: planner.BlockedService},
	}
	wildcard := "*.store.example.com"
	rejected := engine.RouteView{
		RouteStatus: planner.RouteStatus{Hostname: wildcard, Owner: "lxc/210", State: planner.StateRejected, Zone: "example.com",
			Reason: fmt.Sprintf("a wildcard is published only when an allowHosts pattern names it: add %q to allowHosts", wildcard)},
		Guest: &engine.GuestView{GuestRef: guest(model.KindLXC, 210), Name: "store"},
	}
	status := engine.RouteView{
		RouteStatus: planner.RouteStatus{Hostname: "status.example.com", Owner: "manual/status", State: planner.StateActive,
			Service: "http://10.0.5.20:9000", Zone: "example.com"},
		Account: www.Account, Rule: &planner.IngressRule{Hostname: "status.example.com", Service: "http://10.0.5.20:9000"},
	}
	notes := served(www, "notes.example.info", guest(model.KindLXC, 221), "notes", "http://10.0.20.51:8080", "veth221i0")
	notes.State, notes.Reason, notes.Service, notes.Zone, notes.Account = engine.RouteFrozen, "account frozen: "+frozenWhy, "", "example.info", "acc3"
	st.Routes = append(st.Routes, app, git, held, rejected, status, notes,
		served(www, "grafana.example.com", guest(model.KindLXC, 205), "grafana", "http://10.0.0.20:3000", "veth205i0"),
		served(www, "store.example.com", guest(model.KindLXC, 210), "store", "http://10.0.0.40:80", "veth210i0"),
		served(www, "www.store.example.com", guest(model.KindLXC, 210), "store", "http://10.0.0.40:80", "veth210i0"),
		served(www, "vpn.example.com", guest(model.KindLXC, 230), "wg-ui", "http://10.0.0.60:51821", "veth230i0"),
		down(served(www, "monitor.example.com", guest(model.KindQEMU, 105), "monitor", "https://10.0.0.15:8443", "tap105i0"),
			planner.StateUnreachable, reasonNotAnswering),
		down(served(www, "db-admin.example.com", guest(model.KindQEMU, 106), "db-admin", "http://10.0.0.14:8080", "tap106i0"),
			planner.StateWithdrawn, reasonWithdrawn),
		noZone(served(www, "wiki.example.org", guest(model.KindLXC, 220), "wiki", "http://10.0.20.50:8080", "veth220i0"),
			"credential main can list example.org but not read its DNS: grant Zone > DNS > Edit to serve it; "+
				"pco credential check cred1 picks the grant up at once"),
		noZone(served(www, "media.example.io", guest(model.KindQEMU, 130), "jellyfin", "http://10.0.0.70:8096", "tap130i0"),
			"no Cloudflare zone for this hostname in any credential"),
	)
	sortState(&st)
	st.Digest = digestOf(st)

	traffic := &engine.TrafficView{Interval: engine.TrafficInterval.String(), Tunnels: []engine.TunnelTraffic{}}
	for _, c := range st.Connectors {
		version := st.Tunnels[slices.IndexFunc(st.Tunnels, func(tv engine.TunnelView) bool { return tv.ID == c.TunnelID })].Version
		tt := engine.TunnelTraffic{
			TunnelID: c.TunnelID, Node: st.Node, Cloudflared: "2026.8.0", ConfigVersion: version, HAConnections: c.Connections,
			Edges:     []connector.Edge{{Connection: 0, Location: "fra08"}, {Connection: 1, Location: "fra08"}, {Connection: 2, Location: "prg01"}, {Connection: 3, Location: "prg01"}},
			RTTMillis: []float64{11.8, 12.4, 17.9, 18.3},
		}
		for i := range 12 {
			tt.Samples = append(tt.Samples, engine.TrafficSample{
				At: st.FinishedAt.Add(time.Duration(i-11) * engine.TrafficInterval), RPS: 38.2 + float64(i%4) - 1.5, ErrorsPerSec: 0.1, Concurrent: 3,
			})
		}
		traffic.Tunnels = append(traffic.Tunnels, tt)
	}
	traffic.Routes = routeTraffic(st.Routes)
	traffic.RoutesTotal = len(traffic.Routes)

	settings := golden[engine.SettingsView](t, apiGoldens, "settings.json")
	settings.Settings.Admission = st.Admission
	settings.Settings.ManualCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")}

	guests, guestNotes := guestsOf(t, st, golden[[]engine.GuestListView](t, apiGoldens, "guests.json"))
	return files{
		About:     "the populated state of the engine, with the examples of the mockup: a route in every state, traffic, a manual route",
		State:     st,
		Events:    populatedEvents(t, st),
		Traffic:   traffic,
		Settings:  &settings,
		Manual:    golden[[]engine.ManualRouteView](t, apiGoldens, "manual_routes.json"),
		Guests:    guests,
		Notes:     guestNotes,
		Claims:    golden[[]engine.ClaimView](t, engineGoldens, "claims.json"),
		Approvals: golden[[]engine.ApprovalView](t, engineGoldens, "approvals.json"),
	}
}

// served is the route of www served at another hostname, from another guest,
// at service; the guest's NIC is untagged on vmbr0.
func served(www engine.RouteView, host string, ref model.GuestRef, name, service, port string) engine.RouteView {
	r := clone(www)
	addr := netip.MustParseAddrPort(service[strings.Index(service, "://")+3:]).Addr()
	r.Hostname, r.Owner, r.Service, r.Warnings = host, ref.String(), service, nil
	r.Guest = &engine.GuestView{GuestRef: ref, Name: name}
	r.Candidates = []resolve.CandidateResult{{Addr: addr, Source: resolve.FromStatic, OK: true, Level: string(resolve.LevelPort)}}
	r.Rule = &planner.IngressRule{Hostname: host, Service: service}
	r.Path.Bridge, r.Path.VLAN, r.Path.Port = "vmbr0", 0, port
	r.Path.MAC = fmt.Sprintf("bc:24:11:00:%02x:%02x", ref.VMID/256, ref.VMID%256)
	return r
}

// guestsOf is the guest list: the guests of the golden, and every guest the
// state names, each with the routes and issues the state has of it; and the
// Notes of the guests, the one of the golden annotation among them.
func guestsOf(t *testing.T, st engine.State, list []engine.GuestListView) ([]engine.GuestListView, map[string]string) {
	t.Helper()
	notes := map[string]string{}
	lines := map[string][]string{}
	add := func(ref model.GuestRef, name string) {
		if slices.ContainsFunc(list, func(g engine.GuestListView) bool { return g.Ref == ref.String() }) {
			return
		}
		list = append(list, engine.GuestListView{Ref: ref.String(), Name: name, Node: st.Node, Running: true, Tagged: true,
			Identity: fmt.Sprintf("uuid:%d", ref.VMID), Approval: engine.ApprovalApproved})
	}
	for _, r := range st.Routes {
		if r.Guest == nil {
			continue
		}
		add(r.Guest.GuestRef, r.Guest.Name)
		target := ":8080"
		if r.Rule != nil && r.Rule.Service != planner.BlockedService {
			scheme, rest, _ := strings.Cut(r.Rule.Service, "://")
			_, port, _ := strings.Cut(rest, ":")
			target = scheme + "://:" + port
		}
		lines[r.Owner] = append(lines[r.Owner], r.Hostname+" -> "+target)
	}
	for _, u := range st.Unapproved {
		add(u.GuestRef, u.Name)
		for _, host := range u.Hostnames {
			lines[u.String()] = append(lines[u.String()], host+" -> :8080")
		}
	}
	for _, is := range st.Issues {
		if is.Guest.VMID != 0 {
			add(is.Guest, "")
		}
	}
	for i := range list {
		g := &list[i]
		g.Routes = len(slices.DeleteFunc(slices.Clone(st.Routes), func(r engine.RouteView) bool { return r.Owner != g.Ref }))
		g.Issues = len(slices.DeleteFunc(slices.Clone(st.Issues), func(is planner.Issue) bool { return is.Guest.String() != g.Ref }))
		if slices.ContainsFunc(st.Unapproved, func(u engine.UnapprovedGuest) bool { return u.String() == g.Ref }) {
			g.Approval = engine.ApprovalWaiting
		}
		if l := lines[g.Ref]; len(l) > 0 {
			notes[g.Ref] = "```cf-tunnel\n" + strings.Join(l, "\n") + "\n```\n"
		}
	}
	slices.SortFunc(list, func(a, b engine.GuestListView) int { return model.CompareOwners(a.Ref, b.Ref) })

	// The Notes whose block the golden annotation is.
	want := golden[engine.AnnotationView](t, apiGoldens, "annotation.json")
	notes[want.Ref] = "Build server of the team.\nAsk ops before a change.\n\n" + want.Block + "\n"
	block, line := annotation.Block(notes[want.Ref])
	require.Equal(t, want.Block, block)
	require.Equal(t, want.StartLine, line)
	for ref, n := range notes {
		if ref != want.Ref {
			require.Empty(t, annotation.Parse(n).Errors, "the Notes of %s", ref)
		}
	}
	return list, notes
}

// populatedEvents are the events of the golden, after those of the mockup.
func populatedEvents(t *testing.T, st engine.State) []engine.Event {
	at := func(minutes int) time.Time { return st.At.Add(time.Duration(minutes) * time.Minute) }
	events := []engine.Event{
		{At: at(-6), Level: "info", Kind: "rollout", Subject: "pco-abc123", Tunnel: "pco-abc123", Account: "acc1",
			Message: "configuration version 3 runs on 1 connector in account acc1"},
		{At: at(-5), Level: "warn", Kind: "route", Subject: "monitor.example.com", Route: "monitor.example.com", Guest: "qemu/105", Account: "acc1",
			Message: "qemu/105: unreachable (target is not answering)"},
		{At: at(-4), Level: "warn", Kind: "conflict", Subject: "api.example.com", Route: "api.example.com",
			Message: "A 192.0.2.10 in zone example.com is not ours; the hostname is not published"},
		{At: at(-3), Level: "info", Kind: "admin", Subject: "qemu/110", Actor: "alice@pve (ticket)",
			Message: "qemu/110 (gitea) is approved in identity uuid:110"},
		{At: at(-2), Level: "warn", Kind: "route", Subject: "db-admin.example.com", Route: "db-admin.example.com", Guest: "qemu/106", Account: "acc1",
			Message: "qemu/106: withdrawn (identity check failed)"},
		{At: at(-1), Level: "warn", Kind: "route", Subject: "*.store.example.com", Route: "*.store.example.com", Guest: "lxc/210",
			Message: "lxc/210: rejected (a wildcard is published only when an allowHosts pattern names it: add \"*.store.example.com\" to allowHosts)"},
	}
	for _, ev := range golden[[]engine.Event](t, engineGoldens, "events.json") {
		ev.Seq, ev.Boot = 0, ""
		events = append(events, ev)
	}
	return events
}

// holdScenario is populated in a cycle that held: it did not check
// Cloudflare, so no tunnel is known to be as the state shows it.
func holdScenario(populated files) files {
	f := populated.copied()
	f.About = "populated in a cycle that held before it checked Cloudflare: every tunnel is unchecked"
	st := &f.State
	st.Hold = "no writer identity; run pco setup"
	st.Problems = append(st.Problems, st.Hold)
	for i := range st.Tunnels {
		tv := &st.Tunnels[i]
		tv.Unchecked, tv.Verified = true, false
		if tv.Held == "" {
			tv.Held = "not checked in the last cycle: " + st.Hold
		}
	}
	sortState(st)
	st.Digest = digestOf(*st)
	f.Events = append(f.Events, engine.Event{At: st.At, Level: "warn", Kind: "hold", Message: "the cycle holds: " + st.Hold})
	return f
}

// frozenScenario is populated with the freeze of a served zone: one zone
// after the first refusal of its credential, one after the second, and the
// changes that wait for Cloudflare's rate limit.
func frozenScenario(populated files) files {
	f := populated.copied()
	f.About = "the freeze of a served zone: one after the first refusal of its credential, one after the second, and the budget stop"
	st := &f.State
	first := "the token of credential cred2 could not read the DNS of zone example.eu, which it serves"
	second := "credential cred1 can no longer read the DNS of zone example.shop, which it serves: grant it Zone > DNS > Edit there"
	st.Problems = append(st.Problems,
		first+"; account acc5 is left as it is, checking again at 12:10",
		second+"; account acc6 is left as it is until a check finds it readable again, a pin gives the zone to credential cred2, "+
			"which can read it, or pco apply --confirm-deletes lets the zone go",
		reconcile.Waiting{Changes: 23}.Line())
	st.Waiting = append(st.Waiting, engine.Waiting{
		Kind: engine.WaitingZone, Subject: "example.shop", Items: []string{},
		Detail: "credential cred1 can no longer read the DNS of zone example.shop; a confirmation lets the zone go from it, " +
			"and a pin to credential cred2, which can read it, ends the freeze as well",
	})
	slices.SortStableFunc(st.Waiting, func(a, b engine.Waiting) int {
		return strings.Compare(a.Kind+"\x00"+a.Subject, b.Kind+"\x00"+b.Subject)
	})
	data, _ := json.Marshal(st.Waiting)
	sum := sha256.Sum256(data)
	st.Offer = hex.EncodeToString(sum[:8])

	www := st.Routes[slices.IndexFunc(st.Routes, func(r engine.RouteView) bool { return r.Hostname == "www.example.com" })]
	for _, z := range []struct {
		name, id, account, credential, why, host string
		vmid                                     int
		credentials, excluded                    []string
	}{
		{"example.eu", "zone5", "acc5", "cred2", first, "shop.example.eu", 240, []string{"cred2"}, []string{"cred2"}},
		{"example.shop", "zone6", "acc6", "cred1", second, "cart.example.shop", 241, []string{"cred1", "cred2"}, []string{"cred1"}},
	} {
		st.Zones = append(st.Zones, engine.ZoneView{
			Name: z.name, ID: z.id, Status: "active", AccountID: z.account, State: engine.ZoneFrozen,
			Credentials: z.credentials, Stale: []string{}, Excluded: z.excluded, FrozenWhy: z.why,
		})
		st.Tunnels = append(st.Tunnels, engine.TunnelView{
			TunnelState: reconcile.TunnelState{AccountID: z.account, CredentialID: z.credential, Name: "pco-abc123",
				ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", z.vmid), Version: 1, Exists: true},
			Held: "account frozen: " + z.why, LeftAsIs: true,
		})
		r := served(www, z.host, model.GuestRef{Kind: model.KindLXC, VMID: z.vmid}, strings.Split(z.host, ".")[0], fmt.Sprintf("http://10.0.30.%d:8080", z.vmid-200), fmt.Sprintf("veth%di0", z.vmid))
		r.State, r.Reason, r.Service, r.Zone, r.Account = engine.RouteFrozen, "account frozen: "+z.why, "", z.name, z.account
		st.Routes = append(st.Routes, r)
	}
	sortState(st)
	st.Digest = digestOf(*st)
	f.Traffic.Routes = routeTraffic(st.Routes)
	f.Traffic.RoutesTotal = len(f.Traffic.Routes)
	for _, p := range st.Problems {
		if !slices.Contains(populated.State.Problems, p) {
			f.Events = append(f.Events, engine.Event{At: st.At, Level: "warn", Kind: "problem", Message: p})
		}
	}
	return f
}

// egressOffScenario is populated with the egress filter switched off: the
// routes have no figures, and the traffic says why.
func egressOffScenario(populated files) files {
	f := populated.copied()
	f.About = "populated with the egress filter switched off: no figures of the routes, and why"
	f.State.Egress = engine.EgressView{State: engine.EgressOff, Since: f.State.At.Add(-time.Hour)}
	f.State.Digest = digestOf(f.State)
	f.Traffic.Routes, f.Traffic.RoutesTotal, f.Traffic.RoutesWhy = []engine.RouteTraffic{}, 0, whyOff
	f.Events = append(f.Events, engine.Event{At: f.State.Egress.Since, Level: "warn", Kind: "egress",
		Message: "the egress filter is switched off by the admin; the connectors may reach any address"})
	return f
}

// accountID matches the account ids of the golden.
var accountID = regexp.MustCompile(`\bacc([1-4])\b`)

// rogueScenario is populated with connectors pco does not run on a tunnel of
// the install, and connectors of its own that cannot start. Its accounts have
// ids as Cloudflare gives them, so that a command to rotate the secret of a
// tunnel can be composed of one.
func rogueScenario(t *testing.T, populated files) files {
	f := populated.copied()
	f.About = "two connectors pco does not run on a tunnel, one whose token Cloudflare refuses, one whose metrics port is held"
	hexed := func(v any) []byte {
		data, err := json.Marshal(v)
		require.NoError(t, err)
		return accountID.ReplaceAllFunc(data, func(m []byte) []byte {
			n := int(m[3] - '0')
			return []byte(hexID("account", n))
		})
	}
	require.NoError(t, json.Unmarshal(hexed(f.State), &f.State))
	require.NoError(t, json.Unmarshal(hexed(f.Events), &f.Events))
	st := &f.State

	first := st.RogueConnectors[0]
	second := first
	second.ID, second.OriginIP, second.Version, second.Since = "4f8a2c19-7e3d-4b6a-9c05-1d2e3f405162", "203.0.113.24", "2025.11.1", first.Since.Add(-3*time.Hour)
	st.RogueConnectors = append(st.RogueConnectors, second)
	slices.SortFunc(st.RogueConnectors, func(a, b engine.RogueConnector) int { return strings.Compare(a.ID, b.ID) })
	for _, r := range st.RogueConnectors {
		st.Problems = append(st.Problems, fmt.Sprintf("tunnel %s in account %s is served by connector %s, which pco does not run on this node: "+
			"it takes a share of the requests to every hostname of the tunnel; "+
			"unless you run it, rotate the tunnel secret with pco tunnel rotate --account %s", r.Tunnel, r.Account, r.Text(), r.Account))
		f.Events = append(f.Events, engine.Event{At: st.At, Level: "error", Kind: "connector", Subject: r.ID, Tunnel: r.Tunnel, Account: r.Account,
			Message: fmt.Sprintf("connector %s serves tunnel %s in account %s and is not one pco runs on this node", r.Text(), r.Tunnel, r.Account)})
	}
	for i, tv := range st.Tunnels {
		if tv.ID == "" || tv.ID == first.TunnelID {
			continue
		}
		c := connector.Status{TunnelID: tv.ID, Active: true, MetricsAddr: fmt.Sprintf("127.0.0.1:%d", 20301+i), Install: "abc123"}
		if !slices.ContainsFunc(st.Connectors, func(s connector.Status) bool { return s.TokenRefused }) {
			c.TokenRefused = true
			st.Problems = append(st.Problems, fmt.Sprintf("tunnel %s in account %s: Cloudflare refuses the token its connector runs with; "+
				"pco reads the token again when that starts and every five minutes, and pco tunnel rotate gives the tunnel a new secret", tv.Name, tv.AccountID))
		} else {
			c.MetricsPortHeld = true
			st.Problems = append(st.Problems, fmt.Sprintf("tunnel %s in account %s: metrics port %d is held by another process, which keeps its connector from starting; "+
				"the connector gets another port in the first cycle that keeps it running", tv.Name, tv.AccountID, 20301+i))
		}
		st.Connectors = append(st.Connectors, c)
	}
	sortState(st)
	st.Digest = digestOf(*st)
	return f
}

// emptyScenario is the state of a daemon before its first cycle.
func emptyScenario(t *testing.T) files {
	settings := golden[engine.SettingsView](t, apiGoldens, "settings.json")
	settings.Settings = store.DefaultSettings()
	return files{
		About:    "the state of a daemon before its first cycle",
		State:    golden[engine.State](t, engineGoldens, "state_empty.json"),
		Settings: &settings,
	}
}

// firstRunScenario is a daemon set up without a credential, before its first
// cycle: what the first-run wizard begins with. A credential added gets the
// report of the golden's.
func firstRunScenario(t *testing.T) files {
	f := emptyScenario(t)
	f.About = "set up, without a credential and before the first cycle, observe-only: where the first-run wizard begins"
	f.State.Problems = []string{"no Cloudflare credential; add one with pco credential add"}
	f.State.Digest = digestOf(f.State)
	report := golden[engine.State](t, engineGoldens, "state_populated.json").Credentials[0].Report
	f.Report = &report
	return f
}

// untaggedScenario is an install with a credential that serves a zone and no
// route, because tagged guests carry the gate tag and name no hostname, or
// none does. With tagged guests the admission mode is approve.
func untaggedScenario(t *testing.T, populated files, tagged int) files {
	full := populated.State
	st := golden[engine.State](t, engineGoldens, "state_empty.json")
	st.At, st.FinishedAt, st.Node, st.Profile, st.Mode, st.Complete = full.At, full.FinishedAt, full.Node, full.Profile, engine.ModeEnforce, true
	st.Credentials, st.Egress, st.GateTagged = full.Credentials[:1], engine.EgressView{State: engine.EgressOn}, tagged
	st.Zones = slices.DeleteFunc(slices.Clone(full.Zones), func(z engine.ZoneView) bool { return z.State != engine.ZoneServed })
	st.Admission = store.AdmissionTag
	about := "an install whose guests carry no gate tag: no route"
	if tagged > 0 {
		st.Admission = store.AdmissionApprove
		about = fmt.Sprintf("an install in admission mode approve whose %d tagged guests name no hostname: no route", tagged)
	}
	st.Digest = digestOf(st)
	settings := clone(*populated.Settings)
	settings.Settings.Admission = st.Admission
	var guests []engine.GuestListView
	for i := range 5 {
		g := engine.GuestListView{Ref: fmt.Sprintf("qemu/%d", 101+i), Name: fmt.Sprintf("vm-%d", 101+i), Node: st.Node, Running: i != 4,
			Identity: fmt.Sprintf("uuid:%d", 101+i), Approval: engine.ApprovalNotNeeded}
		if i < tagged {
			g.Tagged, g.Approval = true, engine.ApprovalWaiting
		}
		guests = append(guests, g)
	}
	return files{About: about, State: st, Settings: &settings, Guests: guests}
}

// encoded is a scenario as the files of its directory, by name.
func encoded(t *testing.T, f files) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	put := func(name string, v any) {
		data, err := json.MarshalIndent(v, "", "  ")
		require.NoError(t, err)
		out[name] = append(data, '\n')
	}
	put(fileScenario, f)
	if f.Like != "" || f.Generate != "" {
		return out
	}
	put(fileState, f.State)
	for _, o := range []struct {
		name string
		v    any
		ok   bool
	}{
		{fileEvents, f.Events, len(f.Events) > 0}, {fileTraffic, f.Traffic, f.Traffic != nil},
		{fileSettings, f.Settings, f.Settings != nil}, {fileManual, f.Manual, len(f.Manual) > 0},
		{fileGuests, f.Guests, len(f.Guests) > 0}, {fileNotes, f.Notes, len(f.Notes) > 0},
		{fileClaims, f.Claims, len(f.Claims) > 0}, {fileApprovals, f.Approvals, len(f.Approvals) > 0},
		{fileReport, f.Report, f.Report != nil},
	} {
		if o.ok {
			put(o.name, o.v)
		}
	}
	return out
}

func TestTheScenariosAreMadeFromTheGoldens(t *testing.T) {
	made := madeScenarios(t)
	if *update {
		require.NoError(t, os.RemoveAll(scenariosDir))
	}
	for name, f := range made {
		dir := filepath.Join(scenariosDir, name)
		want := encoded(t, f)
		if *update {
			require.NoError(t, os.MkdirAll(dir, 0o755))
			for file, data := range want {
				require.NoError(t, os.WriteFile(filepath.Join(dir, file), data, 0o644))
			}
			continue
		}
		entries, err := os.ReadDir(dir)
		require.NoError(t, err, "go test ./internal/api/apifake -run TestTheScenariosAreMadeFromTheGoldens -update writes the scenarios")
		var have []string
		for _, e := range entries {
			have = append(have, e.Name())
		}
		require.ElementsMatch(t, mapKeys(want), have, "the files of %s", name)
		for file, data := range want {
			got, err := os.ReadFile(filepath.Join(dir, file))
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got), "%s/%s is not what the goldens make: run the test with -update and read the diff", name, file)
		}
	}
	if !*update {
		require.ElementsMatch(t, mapKeys(made), Scenarios(), "the package embeds the scenarios it makes")
	}
}

// copied is a copy of f that shares nothing with it, for a scenario made of
// another.
func (f files) copied() files {
	out := f
	out.State, out.Events, out.Manual, out.Guests = clone(f.State), clone(f.Events), clone(f.Manual), clone(f.Guests)
	out.Notes, out.Claims, out.Approvals = clone(f.Notes), clone(f.Claims), clone(f.Approvals)
	out.Traffic, out.Settings, out.Report = clonePtr(f.Traffic), clonePtr(f.Settings), clonePtr(f.Report)
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := clone(*p)
	return &v
}

// clone copies v down to its last pointer, through its JSON, which the types
// of the API always have.
func clone[T any](v T) T {
	var out T
	data, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	if err != nil {
		panic(fmt.Sprintf("copying a %T: %v", v, err))
	}
	return out
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fieldPaths are the paths of the fields a JSON value has, a list's elements
// as one: "routes[].path.vlan".
func fieldPaths(t *testing.T, v any) map[string]bool {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var doc any
	require.NoError(t, json.Unmarshal(data, &doc))
	out := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out[p] = true
				walk(p, x)
			}
		case []any:
			for _, x := range v {
				walk(prefix+"[]", x)
			}
		}
	}
	walk("", doc)
	return out
}

func TestEveryScenarioCarriesEveryFieldOfItsGolden(t *testing.T) {
	populated := fieldPaths(t, golden[engine.State](t, engineGoldens, "state_populated.json"))
	empty := fieldPaths(t, golden[engine.State](t, engineGoldens, "state_empty.json"))
	for _, c := range []struct {
		name   string
		golden map[string]bool
		except []string // what the scenario leaves out on purpose
	}{
		{"populated", populated, []string{"hold", "egress.since"}},
		{"reader", populated, []string{"hold", "egress.since"}},
		{"hold", populated, []string{"egress.since"}},
		{"frozen", populated, []string{"hold", "egress.since"}},
		{"egress-off", populated, []string{"hold"}},
		{"rogue", populated, []string{"hold", "egress.since"}},
		{"large", populated, []string{"hold", "egress.since"}},
		{"outage", populated, []string{"hold", "egress.since"}},
		{"wide", populated, []string{"hold", "egress.since"}},
		{"empty", empty, nil},
		{"first-run", empty, nil},
		{"untagged", empty, nil},
		{"tagged-empty", empty, nil},
	} {
		f, err := builtinFiles(c.name)
		require.NoError(t, err, c.name)
		has := fieldPaths(t, f.State)
		var missing []string
		for p := range c.golden {
			if !has[p] && !slices.Contains(c.except, p) {
				missing = append(missing, p)
			}
		}
		slices.Sort(missing)
		require.Empty(t, missing, "%s lacks fields of its golden", c.name)
	}
}

func TestTheScenariosAreAsTheyAreDescribed(t *testing.T) {
	load := func(name string) files {
		f, err := builtinFiles(name)
		require.NoError(t, err)
		return f
	}
	routes := func(f files, keep func(engine.RouteView) bool) int {
		return len(slices.DeleteFunc(slices.Clone(f.State.Routes), func(r engine.RouteView) bool { return !keep(r) }))
	}
	notActive := func(r engine.RouteView) bool { return r.State != planner.StateActive }
	zonesOf := func(f files) map[string]bool {
		out := map[string]bool{}
		for _, r := range f.State.Routes {
			if r.Zone != "" {
				out[r.Zone] = true
			}
		}
		return out
	}

	populated := load("populated")
	for _, state := range []planner.RouteState{planner.StateActive, planner.StateUnreachable, planner.StateWithdrawn, planner.StateConflict,
		planner.StateNoZone, planner.StateHeld, planner.StateRejected, engine.RouteFrozen} {
		require.Positive(t, routes(populated, func(r engine.RouteView) bool { return r.State == state }), "populated has a route %s", state)
	}
	require.True(t, slices.ContainsFunc(populated.State.Routes, func(r engine.RouteView) bool {
		return r.State == planner.StateRejected && strings.HasPrefix(r.Hostname, "*.")
	}), "a wildcard no allowHosts pattern names is rejected")
	require.True(t, slices.ContainsFunc(populated.Traffic.Routes, func(r engine.RouteTraffic) bool { return r.Shared == 1 }), "two routes share a target")

	large := load("large")
	require.Len(t, large.State.Routes, 1000)
	require.Equal(t, 40, routes(large, notActive))
	require.Len(t, zonesOf(large), 6)
	require.Len(t, large.Guests, 2000)
	require.Equal(t, routes(large, func(r engine.RouteView) bool { return targetOf(r) != "" }), large.Traffic.RoutesTotal)

	outage := load("outage")
	require.Len(t, outage.State.Routes, 1000)
	require.Equal(t, 600, routes(outage, func(r engine.RouteView) bool { return r.State == planner.StateUnreachable }))
	reasons := map[string]bool{}
	for _, r := range outage.State.Routes {
		if r.State == planner.StateUnreachable {
			reasons[r.Reason] = true
		}
	}
	require.Len(t, reasons, 3)

	wide := load("wide")
	accounts := map[string]bool{}
	for _, z := range wide.State.Zones {
		accounts[z.AccountID] = true
	}
	require.Len(t, wide.State.Zones, 50)
	require.Len(t, accounts, 30)
	require.Len(t, wide.State.Tunnels, 30)
	require.Len(t, wide.State.Routes, 300)

	for _, name := range []string{"large", "outage", "wide"} {
		again, err := builtinFiles(name)
		require.NoError(t, err)
		require.Equal(t, load(name), again, "%s is made the same every time", name)
	}

	rogue := load("rogue")
	require.Len(t, rogue.State.RogueConnectors, 2)
	require.Len(t, slices.DeleteFunc(slices.Clone(rogue.State.Connectors), func(c connector.Status) bool { return !c.TokenRefused }), 1)
	require.Len(t, slices.DeleteFunc(slices.Clone(rogue.State.Connectors), func(c connector.Status) bool { return !c.MetricsPortHeld }), 1)
	for _, tv := range rogue.State.Tunnels {
		require.Regexp(t, `^[0-9a-f]{32}$`, tv.AccountID)
	}

	firstRun := load("first-run")
	require.True(t, firstRun.State.At.IsZero())
	require.Empty(t, firstRun.State.Credentials)
	require.Equal(t, engine.ModeObserve, firstRun.State.Mode)
	require.True(t, firstRun.Settings.Settings.ObserveOnly)
	require.NotEmpty(t, firstRun.State.Problems)
	require.NotNil(t, firstRun.Report, "a credential added in the wizard is checked as the golden's was")

	hold := load("hold")
	require.NotEmpty(t, hold.State.Hold)
	for _, tv := range hold.State.Tunnels {
		require.True(t, tv.Unchecked)
	}

	frozen := load("frozen")
	require.True(t, slices.ContainsFunc(frozen.State.Waiting, func(w engine.Waiting) bool { return w.Subject == "example.shop" }))
	require.True(t, slices.ContainsFunc(frozen.State.Problems, func(p string) bool { return strings.HasSuffix(p, "for "+reconcile.HeldBudget) }))

	off := load("egress-off")
	require.Equal(t, engine.EgressOff, off.State.Egress.State)
	require.Equal(t, whyOff, off.Traffic.RoutesWhy)

	require.Zero(t, load("untagged").State.GateTagged)
	tagged := load("tagged-empty")
	require.Equal(t, 3, tagged.State.GateTagged)
	require.Empty(t, tagged.State.Routes)
	require.Equal(t, store.AdmissionApprove, tagged.State.Admission)
}
