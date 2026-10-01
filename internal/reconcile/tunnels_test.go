package reconcile

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	testInstall = "abc"
	testTunnel  = "pco-abc"
)

var (
	t0 = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)

	// ours is the writer every test runs as unless it says otherwise.
	ours = writerAt(5, "n5")

	app      = planner.IngressRule{Hostname: "app.example.com", Service: "http://10.0.0.5:8080"}
	web      = planner.IngressRule{Hostname: "web.example.com", Service: "https://10.0.0.6:443", OriginServerName: "web.example.com"}
	catchAll = planner.IngressRule{Service: "http_status:404"}
)

func writerAt(generation int, nonce string) planner.Writer {
	return planner.Writer{InstallID: testInstall, Generation: generation, Nonce: nonce}
}

func sentinelOf(w planner.Writer) planner.IngressRule {
	return planner.IngressRule{Hostname: planner.SentinelHostname(w), Service: "http_status:404"}
}

// rulesOf returns rules followed by the sentinel of w and the catch-all, as
// the planner ends the rules of every tunnel.
func rulesOf(w planner.Writer, rules ...planner.IngressRule) []planner.IngressRule {
	return append(slices.Clone(rules), sentinelOf(w), catchAll)
}

func planFor(account, credential string, rules ...planner.IngressRule) planner.TunnelPlan {
	return planner.TunnelPlan{AccountID: account, CredentialID: credential, Name: testTunnel, Rules: rulesOf(ours, rules...)}
}

func newFake(accounts ...string) *cffake.Fake {
	f := cffake.New()
	for _, a := range accounts {
		f.AddAccount(a, a)
	}
	return f
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// writerOf returns a writer callback that always answers us and stored.
func writerOf(us, stored planner.Writer) func() (planner.Writer, planner.Writer, error) {
	return func() (planner.Writer, planner.Writer, error) { return us, stored, nil }
}

func newReconciler(clients Clients, c *clock) *TunnelReconciler {
	return NewTunnelReconciler(clients, writerOf(ours, ours), c.now, zerolog.Nop())
}

// callsTo returns the calls of the fake that go to one of the methods.
func callsTo(f *cffake.Fake, methods ...string) []string {
	var out []string
	for _, call := range f.Calls() {
		if slices.Contains(methods, strings.Fields(call)[0]) {
			out = append(out, call)
		}
	}
	return out
}

// tunnelIn returns the one tunnel of an account.
func tunnelIn(t *testing.T, f *cffake.Fake, account string) cfapi.Tunnel {
	t.Helper()
	tunnels := f.TunnelsIn(account)
	require.Len(t, tunnels, 1, "tunnels in %s", account)
	return tunnels[0]
}

// configIn reads the configuration of the one tunnel of an account. The read
// is in the call log of the fake, so a test looks at the log first.
func configIn(t *testing.T, f *cffake.Fake, account string) cfapi.TunnelConfig {
	t.Helper()
	cfg, err := f.TunnelConfig(context.Background(), account, tunnelIn(t, f, account).ID)
	require.NoError(t, err)
	return cfg
}

// withoutDetail clears the details of actions, for tests that look at the
// rest.
func withoutDetail(actions []Action) []Action {
	out := slices.Clone(actions)
	for i := range out {
		out[i].Detail = ""
	}
	return out
}

func action(kind ActionKind, credential, held string) Action {
	return Action{Kind: kind, Credential: credential, Target: testTunnel, Applied: held == "", Held: held}
}

// spy wraps an API to change what the fake cannot: a lookup that misses a
// tunnel that is there, a failing read of the configuration, and something
// that happens right after a configuration write.
type spy struct {
	cfapi.API

	findMisses int    // the first lookups answer "not found"
	configErr  error  // what TunnelConfig answers
	afterPut   func() // runs after each configuration write that succeeded
	puts       int    // configuration writes made through the spy
}

func (s *spy) FindTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, bool, error) {
	if s.findMisses > 0 {
		s.findMisses--
		return cfapi.Tunnel{}, false, nil
	}
	return s.API.FindTunnel(ctx, accountID, name)
}

func (s *spy) TunnelConfig(ctx context.Context, accountID, tunnelID string) (cfapi.TunnelConfig, error) {
	if s.configErr != nil {
		return cfapi.TunnelConfig{}, s.configErr
	}
	return s.API.TunnelConfig(ctx, accountID, tunnelID)
}

func (s *spy) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	s.puts++
	v, err := s.API.PutTunnelConfig(ctx, accountID, tunnelID, rules)
	if err == nil && s.afterPut != nil {
		s.afterPut()
	}
	return v, err
}

func TestJudgeWriter(t *testing.T) {
	other := func(generation int, nonce string) planner.Writer {
		return planner.Writer{InstallID: "xyz", Generation: generation, Nonce: nonce}
	}
	cases := []struct {
		name     string
		remote   planner.Writer
		remoteOK bool
		stored   planner.Writer
		want     WriterVerdict
	}{
		{"no remote sentinel", planner.Writer{}, false, ours, WriterProceed},
		{"remote generation lower", writerAt(4, "n4"), true, ours, WriterProceed},
		{"remote generation zero", writerAt(0, "n0"), true, ours, WriterProceed},
		{"equal generation, our nonce", writerAt(5, "n5"), true, ours, WriterProceed},
		{"equal generation, other nonce", writerAt(5, "zz"), true, ours, WriterForeign},
		{"remote higher, stored above it", writerAt(7, "n7"), true, writerAt(8, "n8"), WriterStale},
		{"remote higher, stored is its writer", writerAt(7, "n7"), true, writerAt(7, "n7"), WriterStale},
		{"remote higher, stored at its generation with another nonce", writerAt(7, "n7"), true, writerAt(7, "zz"), WriterForeign},
		{"remote higher, stored below it", writerAt(7, "n7"), true, writerAt(6, "n6"), WriterForeign},
		{"remote higher, stored is us", writerAt(7, "n7"), true, ours, WriterForeign},
		{"another install, higher generation", other(9, "n9"), true, writerAt(7, "n7"), WriterProceed},
		{"another install, equal generation and other nonce", other(5, "zz"), true, ours, WriterProceed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, JudgeWriter(ours, tc.remote, tc.remoteOK, tc.stored))
		})
	}
}

func TestEqualIngress(t *testing.T) {
	insecure := app
	insecure.NoTLSVerify = true
	renamed := web
	renamed.OriginServerName = "origin.example.com"
	moved := app
	moved.Service = "http://10.0.0.7:8080"
	blocking := catchAll
	blocking.Service = "http_status:503"

	cases := []struct {
		name string
		a, b []planner.IngressRule
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and empty", nil, []planner.IngressRule{}, true},
		{"same rules", rulesOf(ours, app, web), rulesOf(ours, app, web), true},
		{"order differs", rulesOf(ours, app, web), rulesOf(ours, web, app), false},
		{
			"empty options equal absent ones",
			[]planner.IngressRule{{Hostname: app.Hostname, Service: app.Service, OriginServerName: "", HTTPHostHeader: "", MatchSNIToHost: false, NoTLSVerify: false}, catchAll},
			[]planner.IngressRule{{Hostname: app.Hostname, Service: app.Service}, catchAll},
			true,
		},
		{"option set on one side", rulesOf(ours, insecure), rulesOf(ours, app), false},
		{"option value differs", rulesOf(ours, renamed), rulesOf(ours, web), false},
		{"service differs", rulesOf(ours, moved), rulesOf(ours, app), false},
		{"sentinel differs", rulesOf(writerAt(4, "n4"), app), rulesOf(ours, app), false},
		{"extra rule", rulesOf(ours, app, web), rulesOf(ours, app), false},
		{"catch-all missing", []planner.IngressRule{app, sentinelOf(ours)}, rulesOf(ours, app), false},
		{"catch-all answers otherwise", []planner.IngressRule{app, sentinelOf(ours), blocking}, rulesOf(ours, app), false},
		{"catch-all not last", []planner.IngressRule{catchAll, app}, []planner.IngressRule{app, catchAll}, false},
		{"empty and catch-all only", nil, []planner.IngressRule{catchAll}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, EqualIngress(tc.a, tc.b))
			require.Equal(t, tc.want, EqualIngress(tc.b, tc.a), "reversed")
		})
	}
}

func TestTunnelCreateAndPut(t *testing.T) {
	f := newFake("acct1")
	var asked int
	writer := func() (planner.Writer, planner.Writer, error) {
		asked++
		return ours, ours, nil
	}
	r := NewTunnelReconciler(Clients{"cred1": f}, writer, (&clock{t0}).now, zerolog.Nop())

	res := r.Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	id := tunnelIn(t, f, "acct1").ID
	require.Equal(t, []string{
		"FindTunnel acct1 pco-abc",
		"CreateTunnel acct1 pco-abc",
		"TunnelConfig acct1 " + id,
		"PutTunnelConfig acct1 " + id,
		"TunnelConfig acct1 " + id,
	}, f.Calls())
	require.Equal(t, []Action{action(CreateTunnel, "cred1", ""), action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: id, Version: 1, Exists: true}}, res.Tunnels)
	require.Equal(t, 2, asked, "once for the run and once right before the write")
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelNoOpWhenEqual(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, res.Actions)
	require.Equal(t, []string{"FindTunnel acct1 pco-abc", "TunnelConfig acct1 " + tun.ID}, f.Calls())
	require.Equal(t, []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 1, Exists: true}}, res.Tunnels)
}

func TestTunnelForeignConfigRewritten(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	f.SetForeign("acct1", tun.ID, true)

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, "settings pco does not manage")
	require.Len(t, callsTo(f, "PutTunnelConfig"), 1)
	require.Equal(t, 2, res.Tunnels[0].Version)
	cfg := configIn(t, f, "acct1")
	require.False(t, cfg.Foreign)
	require.Equal(t, rulesOf(ours, app), cfg.Ingress)
}

func TestTunnelExternalEditOverwritten(t *testing.T) {
	f := newFake("acct1")
	edited := planner.IngressRule{Hostname: "added.example.com", Service: "http://10.9.9.9:80"}
	f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app, edited))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelStaleWriterStops(t *testing.T) {
	f := newFake("acct1", "acct2")
	newer := writerAt(7, "n7")
	f.SeedTunnel("acct1", testTunnel, rulesOf(newer, app))
	r := NewTunnelReconciler(Clients{"cred1": f}, writerOf(ours, newer), (&clock{t0}).now, zerolog.Nop())

	res := r.Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", web),
	}, nil, Enforce)

	require.Equal(t, WriterStale, res.Verdict)
	require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
	require.Empty(t, f.TunnelsIn("acct2"))
	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1")
	require.Contains(t, res.Problems[0], "stale")
	require.Equal(t, []Action{action(PutConfig, "cred1", "stale writer")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(newer, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelForeignWriterStops(t *testing.T) {
	f := newFake("acct1", "acct2")
	twin := writerAt(5, "zz")
	f.SeedTunnel("acct1", testTunnel, rulesOf(twin, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", web),
	}, nil, Enforce)

	require.Equal(t, WriterForeign, res.Verdict)
	require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
	for _, call := range f.Calls() {
		require.NotContains(t, call, "acct2", "the run must stop before the next tunnel")
	}
	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1")
	require.Contains(t, res.Problems[0], "another installation")
	require.Equal(t, []Action{action(PutConfig, "cred1", "foreign writer")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(twin, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelObserveMode(t *testing.T) {
	f := newFake("acct1", "acct2")
	drifted := rulesOf(writerAt(3, "n3"), app)
	tun := f.SeedTunnel("acct2", testTunnel, drifted)
	writes := 0
	writer := func() (planner.Writer, planner.Writer, error) {
		writes++
		return ours, ours, nil
	}
	r := NewTunnelReconciler(Clients{"cred1": f}, writer, (&clock{t0}).now, zerolog.Nop())

	res := r.Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred1", app),
	}, nil, Observe)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{
		"FindTunnel acct1 pco-abc",
		"FindTunnel acct2 pco-abc",
		"TunnelConfig acct2 " + tun.ID,
	}, f.Calls())
	require.Equal(t, []Action{
		action(CreateTunnel, "cred1", "observe mode"),
		action(PutConfig, "cred1", "observe mode"),
		action(PutConfig, "cred1", "observe mode"),
	}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, "acct1")
	require.Contains(t, res.Actions[2].Detail, "acct2")
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel},
		{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 1, Exists: true},
	}, res.Tunnels)
	require.Equal(t, 1, writes, "the writer is read once, as nothing is written")
	require.Empty(t, f.TunnelsIn("acct1"))
	require.Equal(t, drifted, configIn(t, f, "acct2").Ingress)
}

func TestTunnelPutRateLimited(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", app)}

	res := r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	id := tunnelIn(t, f, "acct1").ID
	_, err := f.PutTunnelConfig(ctx, "acct1", id, rulesOf(ours, web))
	require.NoError(t, err)

	c.t = t0.Add(10 * time.Second)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "rate limit: next write in 5s")}, withoutDetail(res.Actions))
	require.Len(t, callsTo(f, "PutTunnelConfig"), 2, "the first run's write and the edit")
	require.Equal(t, 2, res.Tunnels[0].Version)

	c.t = t0.Add(15 * time.Second)
	res = r.Run(ctx, plans, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, 3, res.Tunnels[0].Version)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelRateLimitIsPerTunnel(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1", "acct2")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)

	res := r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)
	require.Empty(t, res.Problems)

	c.t = t0.Add(time.Second)
	res = r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}, nil, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(CreateTunnel, "cred1", ""), action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct2").Ingress)
}

func TestTunnelUnplannedKnownEmptied(t *testing.T) {
	f := newFake("acct1", "acct2", "acct3")
	tun := f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app, web))
	known := map[string]string{"acct3": "cred1", "acct2": "cred1", "acct1": "cred1"}

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct3", "cred1", web)}, known, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, f.TunnelsIn("acct1"), "a known account without a tunnel gets none")
	created := tunnelIn(t, f, "acct3")
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel},
		{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 2, Exists: true},
		{AccountID: "acct3", CredentialID: "cred1", Name: testTunnel, ID: created.ID, Version: 1, Exists: true},
	}, res.Tunnels)
	require.Equal(t, []Action{
		action(PutConfig, "cred1", ""),
		action(CreateTunnel, "cred1", ""),
		action(PutConfig, "cred1", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, []planner.IngressRule{sentinelOf(ours), catchAll}, configIn(t, f, "acct2").Ingress)
	require.Equal(t, rulesOf(ours, web), configIn(t, f, "acct3").Ingress)
}

func TestTunnelFailureOnOneAccount(t *testing.T) {
	rateLimited := &cfapi.Error{Status: http.StatusTooManyRequests, Message: "Too many requests", RetryAfter: time.Minute}
	cases := []struct {
		name  string
		setup func(f *cffake.Fake) cfapi.API // the client of the account that fails
	}{
		{"tunnel read denied", func(f *cffake.Fake) cfapi.API {
			f.Deny("tunnel.read")
			return f
		}},
		{"create denied", func(f *cffake.Fake) cfapi.API {
			f.Deny("tunnel.write")
			return f
		}},
		{"write denied", func(f *cffake.Fake) cfapi.API {
			f.SeedTunnel("acct1", testTunnel, rulesOf(ours, web))
			f.Deny("tunnel.write")
			return f
		}},
		{"configuration not found", func(f *cffake.Fake) cfapi.API {
			f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
			return &spy{API: f, configErr: &cfapi.Error{Status: http.StatusNotFound, Message: "Not Found"}}
		}},
		{"write rate limited", func(f *cffake.Fake) cfapi.API {
			f.SeedTunnel("acct1", testTunnel, rulesOf(ours, web))
			f.FailNext("tunnel.write", 1, rateLimited)
			return f
		}},
		{"transport failure", func(f *cffake.Fake) cfapi.API {
			f.FailNext("tunnel.read", 1, errors.New("connection reset by peer"))
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failing := newFake("acct1")
			healthy := newFake("acct2")
			clients := Clients{"cred1": tc.setup(failing), "cred2": healthy}

			res := newReconciler(clients, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred2", app),
			}, nil, Enforce)

			require.Len(t, res.Problems, 1, "problems: %v", res.Problems)
			require.Contains(t, res.Problems[0], "pco-abc in account acct1")
			require.Equal(t, WriterProceed, res.Verdict)
			require.Equal(t, rulesOf(ours, app), configIn(t, healthy, "acct2").Ingress)
			require.Equal(t, "acct2", res.Tunnels[len(res.Tunnels)-1].AccountID)
			require.True(t, res.Tunnels[len(res.Tunnels)-1].Exists)
		})
	}
}

func TestTunnelUnknownTunnelLeftOut(t *testing.T) {
	f := newFake("acct1")
	f.Deny("tunnel.read")

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), nil, map[string]string{"acct1": "cred1"}, Enforce)

	require.Len(t, res.Problems, 1)
	require.Empty(t, res.Tunnels, "a tunnel whose lookup failed is not known to be missing")
	require.Empty(t, res.Actions)
}

func TestTunnelVerifyAfterPutMismatch(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, f *cffake.Fake, id string)
	}{
		{"rules replaced", func(t *testing.T, f *cffake.Fake, id string) {
			_, err := f.PutTunnelConfig(context.Background(), "acct1", id, rulesOf(ours, web))
			require.NoError(t, err)
		}},
		{"unmanaged settings added", func(_ *testing.T, f *cffake.Fake, id string) {
			f.SetForeign("acct1", id, true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			tun := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app))
			s := &spy{API: f}
			s.afterPut = func() { tc.change(t, f, tun.ID) }

			res := newReconciler(Clients{"cred1": s}, &clock{t0}).
				Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

			require.Equal(t, []string{"config changed under us on pco-abc in account acct1"}, res.Problems)
			require.Equal(t, 1, s.puts, "no second write in the same run")
			require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
			require.Equal(t, configIn(t, f, "acct1").Version, res.Tunnels[0].Version)
		})
	}
}

func TestTunnelVerifyReadFails(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(3, "n3"), app))
	s := &spy{API: f}
	s.afterPut = func() { s.configErr = errors.New("connection reset by peer") }

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1: reading the configuration back")
	require.Equal(t, []Action{action(PutConfig, "cred1", "")}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tun.ID, Version: 1, Exists: true}},
		res.Tunnels, "the version of the last read that worked")
}

func TestTunnelCreateConflictUnresolved(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *cffake.Fake) *spy
		want  string
	}{
		{"lookup fails", func(f *cffake.Fake) *spy {
			f.Deny("tunnel.read")
			return &spy{API: f, findMisses: 1}
		}, "finding the tunnel after its name was taken"},
		{"still not found", func(f *cffake.Fake) *spy {
			return &spy{API: f, findMisses: 2}
		}, "no tunnel of that name is found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			f.SeedTunnel("acct1", testTunnel, nil)
			healthy := newFake("acct2")

			res := newReconciler(Clients{"cred1": tc.setup(f), "cred2": healthy}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred2", app),
			}, nil, Enforce)

			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "pco-abc in account acct1")
			require.Contains(t, res.Problems[0], tc.want)
			require.Len(t, f.TunnelsIn("acct1"), 1)
			require.Empty(t, callsTo(f, "PutTunnelConfig"))
			require.Equal(t, []Action{
				action(CreateTunnel, "cred1", "tunnel already exists"),
				action(CreateTunnel, "cred2", ""),
				action(PutConfig, "cred2", ""),
			}, withoutDetail(res.Actions))
			require.Equal(t, []TunnelState{
				{AccountID: "acct2", CredentialID: "cred2", Name: testTunnel, ID: tunnelIn(t, healthy, "acct2").ID, Version: 1, Exists: true},
			}, res.Tunnels, "acct1 is left out: its name is taken by a tunnel the run cannot see")
		})
	}
}

func TestTunnelSeveralSentinels(t *testing.T) {
	newer, twin := writerAt(7, "n7"), writerAt(5, "zz")
	cases := []struct {
		name  string
		rules []planner.IngressRule
		want  WriterVerdict
	}{
		{"newer then foreign", []planner.IngressRule{app, sentinelOf(newer), sentinelOf(twin), catchAll}, WriterForeign},
		{"foreign then newer", []planner.IngressRule{app, sentinelOf(twin), sentinelOf(newer), catchAll}, WriterForeign},
		{"older then newer", []planner.IngressRule{app, sentinelOf(writerAt(3, "n3")), sentinelOf(newer), catchAll}, WriterStale},
		{"another install then ours", []planner.IngressRule{app, {Hostname: "g9.n9.pco-xyz.invalid", Service: "http_status:404"}, sentinelOf(ours), catchAll}, WriterProceed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			f.SeedTunnel("acct1", testTunnel, tc.rules)
			r := NewTunnelReconciler(Clients{"cred1": f}, writerOf(ours, newer), (&clock{t0}).now, zerolog.Nop())

			res := r.Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

			require.Equal(t, tc.want, res.Verdict)
			require.Equal(t, tc.want == WriterProceed, len(callsTo(f, "PutTunnelConfig")) == 1)
		})
	}
}

func TestTunnelCreateConflictFindsTunnel(t *testing.T) {
	f := newFake("acct1")
	tun := f.SeedTunnel("acct1", testTunnel, nil)
	s := &spy{API: f, findMisses: 1}

	res := newReconciler(Clients{"cred1": s}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{
		"CreateTunnel acct1 pco-abc",
		"FindTunnel acct1 pco-abc",
		"TunnelConfig acct1 " + tun.ID,
		"PutTunnelConfig acct1 " + tun.ID,
		"TunnelConfig acct1 " + tun.ID,
	}, f.Calls())
	require.Equal(t, []Action{
		action(CreateTunnel, "cred1", "tunnel already exists"),
		action(PutConfig, "cred1", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, tun.ID, res.Tunnels[0].ID)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

func TestTunnelWriterGuards(t *testing.T) {
	newer := writerAt(7, "n7")
	cases := []struct {
		name    string
		answers []planner.Writer // us and stored for each call in turn
		err     []error          // error for each call in turn
		remote  planner.Writer   // sentinel of the seeded configuration of acct1
		calls   bool             // whether any call reaches Cloudflare
		verdict WriterVerdict
	}{
		{name: "identity unreadable", answers: []planner.Writer{ours, ours}, err: []error{errors.New("leader.json: no such file")}, remote: writerAt(3, "n3")},
		{name: "identity invalid", answers: []planner.Writer{{InstallID: "ABC", Generation: 5, Nonce: "n5"}, ours}, remote: writerAt(3, "n3")},
		{name: "identity unreadable before the write", answers: []planner.Writer{ours, ours, ours, ours}, err: []error{nil, errors.New("lease lost")}, remote: writerAt(3, "n3"), calls: true},
		{name: "identity changed before the write", answers: []planner.Writer{ours, ours, writerAt(6, "n6"), writerAt(6, "n6")}, remote: writerAt(3, "n3"), calls: true},
		{name: "stored re-read before the write", answers: []planner.Writer{ours, ours, ours, newer}, remote: newer, calls: true, verdict: WriterStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1", "acct2")
			f.SeedTunnel("acct1", testTunnel, rulesOf(tc.remote, app))
			call := 0
			writer := func() (planner.Writer, planner.Writer, error) {
				i := call
				call++
				var err error
				if i < len(tc.err) {
					err = tc.err[i]
				}
				return tc.answers[2*i], tc.answers[2*i+1], err
			}
			r := NewTunnelReconciler(Clients{"cred1": f}, writer, (&clock{t0}).now, zerolog.Nop())

			res := r.Run(context.Background(), []planner.TunnelPlan{
				planFor("acct1", "cred1", app),
				planFor("acct2", "cred1", app),
			}, nil, Enforce)

			require.Len(t, res.Problems, 1, "problems: %v", res.Problems)
			require.Equal(t, tc.verdict, res.Verdict)
			require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
			require.Equal(t, tc.calls, len(f.Calls()) > 0, "calls: %v", f.Calls())
			for _, c := range f.Calls() {
				require.NotContains(t, c, "acct2", "the run must stop before the next tunnel")
			}
		})
	}
}

func TestTunnelPlanWithoutOurSentinel(t *testing.T) {
	f := newFake("acct1")
	p := planFor("acct1", "cred1", app)
	p.Rules = rulesOf(writerAt(4, "n4"), app)

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{p}, nil, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "pco-abc in account acct1")
	require.Empty(t, f.Calls())
}

func TestTunnelMissingClient(t *testing.T) {
	f := newFake("acct2")

	res := newReconciler(Clients{"cred2": f}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{
		planFor("acct1", "cred1", app),
		planFor("acct2", "cred2", app),
	}, nil, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "cred1")
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct2").Ingress)
}
