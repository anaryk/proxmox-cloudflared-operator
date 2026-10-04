package cffake_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// This file is the contract between the client and the fake: every method of
// cfapi.API runs once against the fake itself and once through the real client
// against the handler in front of an identical fake, and the two must agree on
// the values, on how an error is to be classified and on what the fake holds
// afterwards.

const (
	acct  = "acct1"
	zone  = "zone1"
	token = "contract-token-secret"

	// Ids that cannot be written into a path as they are.
	oddAccount = "ac t?%#"
	oddZone    = "z 2?x#y%"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, time.March, 4, 5, 6, 7, 123456789, time.UTC)
)

// call runs one or several methods of the API and returns what they returned.
type call func(api cfapi.API) (any, error)

// seedFunc fills a fake and returns the call to run against it, which may
// refer to what was seeded.
type seedFunc func(f *cffake.Fake) call

type scenario struct {
	name   string
	method string           // the method of cfapi.API the scenario is about
	op     string           // the operation of the method, when failures are to be injected into it
	only   []int            // the statuses to inject; nil for all
	totals bool             // the answer depends on how a tunnel listing is read, see readings
	plain  bool             // the fake fails with an error that is not one of the API, which crosses the wire as a 500
	is     func(error) bool // what the call must fail with; nil when it must succeed
	seed   seedFunc
}

func ok(method, name string, seed seedFunc) scenario {
	return scenario{name: name, method: method, seed: seed}
}

func refused(method, name string, is func(error) bool, seed seedFunc) scenario {
	return scenario{name: name, method: method, is: is, seed: seed}
}

func (s scenario) injecting(op string) scenario {
	s.op = op
	return s
}

// listing marks a scenario that is run once for each reading of the tunnel
// listings, the others once.
func (s scenario) listing() scenario {
	s.totals = true
	return s
}

// statuses limits the statuses of the errors injected into the scenario.
func (s scenario) statuses(statuses ...int) scenario {
	s.only = statuses
	return s
}

// listed is a scenario that reads a listing of tunnels.
func listed(method, name string, seed seedFunc) scenario {
	return ok(method, name, seed).listing()
}

func isInvalid(err error) bool { return errors.Is(err, cfapi.ErrInvalidArgument) }
func isError(err error) bool   { return err != nil }

func isStatus(status int) func(error) bool {
	return func(err error) bool {
		var apiErr *cfapi.Error
		return errors.As(err, &apiErr) && apiErr.Status == status
	}
}

// readings are the ways a server can answer a tunnel listing that the client
// has to cope with: total_count of every tunnel or of the filtered result,
// and a server that ignores is_deleted.
var readings = []struct {
	name string
	opts []cffake.Option
}{
	{"total of every tunnel", nil},
	{"total of the filtered result", []cffake.Option{cffake.WithFilteredTunnelTotals()}},
	{"is_deleted ignored", []cffake.Option{cffake.WithIgnoredIsDeleted()}},
}

func TestContract(t *testing.T) {
	for _, sc := range allScenarios() {
		t.Run(sc.method+"/"+sc.name, func(t *testing.T) {
			if !sc.totals {
				sc.check(t)
				return
			}
			for _, reading := range readings {
				t.Run(reading.name, func(t *testing.T) { sc.check(t, reading.opts...) })
			}
		})
	}
}

// allScenarios is every scenario and, for each one that names an operation,
// the failures the fake can be made to answer with.
func allScenarios() []scenario {
	var all []scenario
	for _, group := range [][]scenario{
		verifyScenarios(), accountScenarios(), zoneScenarios(),
		findTunnelScenarios(), tunnelsScenarios(), createTunnelScenarios(), deleteTunnelScenarios(),
		tokenScenarios(), rotateScenarios(), cleanUpScenarios(), configScenarios(), putConfigScenarios(), connectorScenarios(),
		recordsScenarios(), createRecordScenarios(), updateRecordScenarios(), deleteRecordScenarios(),
	} {
		for _, sc := range group {
			all = append(all, sc)
			if sc.op != "" {
				all = append(all, failures(sc)...)
			}
		}
	}
	return all
}

// failures makes the scenarios that run sc with its operation denied and with
// each of the errors a test can inject into it.
func failures(sc scenario) []scenario {
	out := []scenario{{
		name: sc.name + ", denied", method: sc.method, is: cfapi.IsAuth,
		seed: func(f *cffake.Fake) call {
			c := sc.seed(f)
			f.Deny(sc.op)
			return c
		},
	}}
	inject := func(name string, is func(error) bool, err error) {
		var apiErr *cfapi.Error
		out = append(out, scenario{
			name: sc.name + ", " + name, method: sc.method, is: is, plain: !errors.As(err, &apiErr),
			seed: func(f *cffake.Fake) call {
				c := sc.seed(f)
				f.FailNext(sc.op, 1, err)
				return c
			},
		})
	}
	statuses := sc.only
	if statuses == nil {
		statuses = []int{400, 401, 403, 404, 409, 500, 502, 503}
	}
	for _, status := range statuses {
		inject(fmt.Sprintf("injected %d", status), isStatus(status),
			&cfapi.Error{Status: status, Codes: []int{7001, 7002}, Message: fmt.Sprintf("injected %d", status)})
	}
	inject("rate limited for 30s", cfapi.IsRateLimited,
		&cfapi.Error{Status: http.StatusTooManyRequests, Codes: []int{971}, Message: "slow down", RetryAfter: 30 * time.Second})
	inject("rate limited without a time", cfapi.IsRateLimited,
		&cfapi.Error{Status: http.StatusTooManyRequests, Message: "slow down"})
	inject("a plain error", isError, errors.New("boom"))
	return out
}

func (sc scenario) check(t *testing.T, opts ...cffake.Option) {
	t.Helper()
	direct, run := seeded(sc.seed)
	want, wantErr := run(direct)
	if sc.is == nil {
		require.NoError(t, wantErr, "the scenario must succeed on the fake itself")
	} else {
		require.True(t, sc.is(wantErr), "the fake failed with %v", wantErr)
	}

	served, run := seeded(sc.seed)
	got, gotErr := run(client(t, served, opts...))

	if sc.is != nil {
		require.True(t, sc.is(gotErr), "the client failed with %v", gotErr)
	}
	requireSameError(t, wantErr, gotErr, sc.plain)
	require.Equal(t, want, got)
	require.Equal(t, snapshot(t, direct), snapshot(t, served), "what the fake holds afterwards")
}

// seeded makes a fake on a clock in UTC, the zone the wire writes its times in:
// a time that comes back through the client is the same instant, but only in
// UTC is it the same value.
func seeded(seed seedFunc) (*cffake.Fake, call) {
	f := cffake.New()
	f.SetNow(func() time.Time { return t0 })
	return f, seed(f)
}

// client is the real client, pointed at the handler in front of f.
func client(t *testing.T, f *cffake.Fake, opts ...cffake.Option) *cfapi.Client {
	t.Helper()
	srv := httptest.NewServer(cffake.Handler(f, opts...))
	t.Cleanup(srv.Close)
	c, err := cfapi.New(cfapi.Options{
		BaseURL: srv.URL + "/client/v4",
		Token:   token,
		Limiter: cfapi.NewLimiter(1_000_000, time.Minute, 1000, nil),
	})
	require.NoError(t, err)
	return c
}

// requireSameError holds got to the classification of want: the answers the
// callers of the client branch on, and for an error of the API the status, the
// message, the retry time and, when the fake says any, the codes. An error that
// is not one of the API stays so, and is worded the same, by the client and
// the fake, which is how a handler that answered it with a 500 would show. When
// the fake was made to fail with such an error, plain says so, and the wire
// carries it as a 500 that has its message.
func requireSameError(t *testing.T, want, got error, plain bool) {
	t.Helper()
	require.Equal(t, want == nil, got == nil, "want %v, got %v", want, got)
	if want == nil {
		return
	}
	for name, is := range map[string]func(error) bool{
		"IsNotFound": cfapi.IsNotFound, "IsConflict": cfapi.IsConflict, "IsAuth": cfapi.IsAuth,
		"IsRateLimited": cfapi.IsRateLimited, "ErrInvalidArgument": isInvalid,
	} {
		require.Equal(t, is(want), is(got), "%s: want %v, got %v", name, want, got)
	}
	var wantAPI, gotAPI *cfapi.Error
	if !errors.As(want, &wantAPI) {
		if plain {
			require.ErrorAs(t, got, &gotAPI)
			require.Equal(t, http.StatusInternalServerError, gotAPI.Status)
			require.Equal(t, want.Error(), gotAPI.Message)
			return
		}
		require.False(t, errors.As(got, &gotAPI), "the client got an answer of the API for what is no error of it: %v", got)
		require.Contains(t, got.Error(), want.Error(), "the client words it as the fake does")
		return
	}
	require.ErrorAs(t, got, &gotAPI)
	require.Equal(t, wantAPI.Status, gotAPI.Status)
	require.Equal(t, wantAPI.Message, gotAPI.Message)
	if wantAPI.Status == http.StatusTooManyRequests && wantAPI.RetryAfter == 0 {
		// The handler asks for the least the client holds back for.
		require.Equal(t, time.Second, gotAPI.RetryAfter)
	} else {
		require.Equal(t, wantAPI.RetryAfter, gotAPI.RetryAfter)
	}
	if len(wantAPI.Codes) > 0 {
		require.Equal(t, wantAPI.Codes, gotAPI.Codes)
	}
}

type state struct {
	Tokens     map[string]string // the run token of each tunnel, by id
	Tunnels    map[string][]cfapi.Tunnel
	Deleted    map[string][]cfapi.Tunnel
	Configs    map[string]cfapi.TunnelConfig
	Connectors map[string][]cfapi.Connector
	Records    map[string][]cfapi.Record
}

// snapshot is everything a call can change in f. Denials are lifted first, so
// that reading the state is never refused.
func snapshot(t *testing.T, f *cffake.Fake) state {
	t.Helper()
	for _, op := range []string{"verify", "accounts", "zones", "tunnel.read", "tunnel.write", "dns.read", "dns.write"} {
		f.Allow(op)
	}
	s := state{
		Tokens:     make(map[string]string),
		Tunnels:    make(map[string][]cfapi.Tunnel),
		Deleted:    make(map[string][]cfapi.Tunnel),
		Configs:    make(map[string]cfapi.TunnelConfig),
		Connectors: make(map[string][]cfapi.Connector),
		Records:    make(map[string][]cfapi.Record),
	}
	for _, account := range []string{acct, "acct2", oddAccount} {
		s.Deleted[account] = f.DeletedTunnelsIn(account)
		for _, tun := range f.TunnelsIn(account) {
			s.Tunnels[account] = append(s.Tunnels[account], tun)
			cfg, err := f.TunnelConfig(ctx, account, tun.ID)
			require.NoError(t, err)
			s.Configs[tun.ID] = cfg
			conns, err := f.Connectors(ctx, account, tun.ID)
			require.NoError(t, err)
			s.Connectors[tun.ID] = conns
			token, err := f.TunnelToken(ctx, account, tun.ID)
			require.NoError(t, err)
			s.Tokens[tun.ID] = token
		}
	}
	for _, z := range []string{zone, "zone2", oddZone} {
		s.Records[z] = f.RecordsIn(z)
	}
	return s
}

func TestEveryMethodOfTheClientIsInTheContract(t *testing.T) {
	covered := make(map[string]bool)
	for _, sc := range allScenarios() {
		if sc.is == nil {
			covered[sc.method] = true
		}
	}
	api := reflect.TypeFor[cfapi.API]()
	var methods []string
	for i := range api.NumMethod() {
		methods = append(methods, api.Method(i).Name)
		require.True(t, covered[api.Method(i).Name], "no scenario in which %s succeeds", api.Method(i).Name)
	}
	for _, sc := range allScenarios() {
		require.Contains(t, methods, sc.method, "scenario %q is about a method the API does not have", sc.name)
	}
}

type foundTunnel struct {
	Tunnel cfapi.Tunnel
	Found  bool
}

func verifyToken() call { return func(a cfapi.API) (any, error) { return a.VerifyToken(ctx) } }
func accounts() call    { return func(a cfapi.API) (any, error) { return a.Accounts(ctx) } }
func zones() call       { return func(a cfapi.API) (any, error) { return a.Zones(ctx) } }

func findTunnel(account, name string) call {
	return func(a cfapi.API) (any, error) {
		tun, found, err := a.FindTunnel(ctx, account, name)
		return foundTunnel{tun, found}, err
	}
}

func tunnels(account, prefix string) call {
	return func(a cfapi.API) (any, error) { return a.Tunnels(ctx, account, prefix) }
}

func createTunnel(account, name string) call {
	return func(a cfapi.API) (any, error) { return a.CreateTunnel(ctx, account, name) }
}

func deleteTunnel(account, id string) call {
	return func(a cfapi.API) (any, error) { return nil, a.DeleteTunnel(ctx, account, id) }
}

func tunnelToken(account, id string) call {
	return func(a cfapi.API) (any, error) { return a.TunnelToken(ctx, account, id) }
}

func tunnelConfig(account, id string) call {
	return func(a cfapi.API) (any, error) { return a.TunnelConfig(ctx, account, id) }
}

func putTunnelConfig(account, id string, rules []planner.IngressRule) call {
	return func(a cfapi.API) (any, error) { return a.PutTunnelConfig(ctx, account, id, rules) }
}

func rotateSecret(account, id string, secret []byte) call {
	return func(a cfapi.API) (any, error) { return nil, a.RotateTunnelSecret(ctx, account, id, secret) }
}

func cleanUp(id string) call {
	return func(a cfapi.API) (any, error) { return nil, a.CleanUpConnections(ctx, acct, id) }
}

func connectors(account, id string) call {
	return func(a cfapi.API) (any, error) { return a.Connectors(ctx, account, id) }
}

func records(zoneID string, f cfapi.RecordFilter) call {
	return func(a cfapi.API) (any, error) { return a.Records(ctx, zoneID, f) }
}

func createRecord(zoneID string, r cfapi.Record) call {
	return func(a cfapi.API) (any, error) { return a.CreateRecord(ctx, zoneID, r) }
}

func updateRecord(zoneID string, r cfapi.Record) call {
	return func(a cfapi.API) (any, error) { return a.UpdateRecord(ctx, zoneID, r) }
}

func deleteRecord(zoneID, id string) call {
	return func(a cfapi.API) (any, error) { return nil, a.DeleteRecord(ctx, zoneID, id) }
}

func std(f *cffake.Fake) {
	f.AddAccount(acct, "Acme")
	f.AddZone(zone, "example.com", acct)
}

func rec(typ, name, content string) cfapi.Record {
	return cfapi.Record{Type: typ, Name: name, Content: content}
}

func cname(name, content string) cfapi.Record {
	r := rec("CNAME", name, content)
	r.Proxied = true
	r.Comment = "pco:abc"
	return r
}

func seedTunnels(f *cffake.Fake, account, prefix string, n int) {
	for i := range n {
		f.SeedTunnel(account, fmt.Sprintf("%s%03d", prefix, i), nil)
	}
}

func verifyScenarios() []scenario {
	soon := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	past := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	with := func(prepare func(f *cffake.Fake)) seedFunc {
		return func(f *cffake.Fake) call {
			std(f)
			prepare(f)
			return verifyToken()
		}
	}
	return []scenario{
		ok("VerifyToken", "active, does not expire", with(func(*cffake.Fake) {})).injecting("verify"),
		ok("VerifyToken", "active with an expiry", with(func(f *cffake.Fake) { f.SetTokenStatus("active", &soon) })),
		ok("VerifyToken", "expired", with(func(f *cffake.Fake) { f.SetTokenStatus("expired", &past) })),
		ok("VerifyToken", "disabled", with(func(f *cffake.Fake) { f.SetTokenStatus("disabled", nil) })),
		// The client goes on to the account form after any refusal of the user
		// form, so an error of 4xx that the fake injects once is made good by
		// it; only the answers that end the check are the same.
		ok("VerifyToken", "owned by an account it sees", with(func(f *cffake.Fake) { f.SetTokenOwner(acct) })).
			injecting("verify").statuses(500, 502, 503),
		ok("VerifyToken", "owned by an account, expired", with(func(f *cffake.Fake) {
			f.SetTokenStatus("expired", &past)
			f.SetTokenOwner(acct)
		})),
		ok("VerifyToken", "owned by the third of its accounts", with(func(f *cffake.Fake) {
			f.AddAccount("acctB", "Bee")
			f.AddAccount("acctC", "Cee")
			f.SetTokenOwner("acctC")
		})),
		refused("VerifyToken", "owned by an account it does not see", cfapi.IsAuth, with(func(f *cffake.Fake) {
			f.SetTokenOwner("acct9")
		})),
		refused("VerifyToken", "owned by an account, when there is none", cfapi.IsAuth, func(f *cffake.Fake) call {
			f.SetTokenOwner("acct9")
			return verifyToken()
		}),
	}
}

func accountScenarios() []scenario {
	many := func(n int) seedFunc {
		return func(f *cffake.Fake) call {
			for i := range n {
				f.AddAccount(fmt.Sprintf("a%03d", i), fmt.Sprintf("Account %d é", i))
			}
			return accounts()
		}
	}
	return []scenario{
		ok("Accounts", "none", many(0)),
		ok("Accounts", "three", many(3)).injecting("accounts"),
		ok("Accounts", "one full page", many(50)),
		ok("Accounts", "one full page and one more", many(51)),
		ok("Accounts", "two full pages", many(100)),
		ok("Accounts", "three pages", many(120)),
	}
}

func zoneScenarios() []scenario {
	many := func(n int) seedFunc {
		return func(f *cffake.Fake) call {
			for i := range 3 {
				f.AddAccount(fmt.Sprintf("a%d", i), fmt.Sprintf("Account %d", i))
			}
			for i := range n {
				f.AddZone(fmt.Sprintf("z%03d", i), fmt.Sprintf("zone-%03d.example", i), fmt.Sprintf("a%d", i%3))
			}
			return zones()
		}
	}
	return []scenario{
		ok("Zones", "none", many(0)),
		ok("Zones", "three", many(3)).injecting("zones"),
		ok("Zones", "one full page", many(50)),
		ok("Zones", "one full page and one more", many(51)),
		ok("Zones", "two full pages", many(100)),
		ok("Zones", "three pages across three accounts", many(120)),
		ok("Zones", "a zone replaced after it was added", func(f *cffake.Fake) call {
			std(f)
			f.AddZone(zone, "example.net", acct)
			return zones()
		}),
	}
}

func findTunnelScenarios() []scenario {
	const oddName = "pco a&b+c=d%e/f é?"
	return []scenario{
		listed("FindTunnel", "one of many", func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "other-", 120)
			f.SeedTunnel(acct, "pco-abc", nil)
			f.SeedTunnel(acct, "pco-abc_probe_1", nil)
			return findTunnel(acct, "pco-abc")
		}).injecting("tunnel.read"),
		listed("FindTunnel", "a tunnel that has connectors", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 1, Connections: 2}})
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "names that only contain it", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "pco-abc-x", nil)
			f.SeedTunnel(acct, "x-pco-abc", nil)
			f.SeedTunnel(acct, "PCO-ABC", nil)
			f.SeedTunnel(acct, "pco-abc", nil)
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "on the last of several pages", func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "pco-abc-x", 120)
			f.SeedTunnel(acct, "pco-abc", nil)
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "none", func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "other-", 120)
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "in another account", func(f *cffake.Fake) call {
			std(f)
			f.AddAccount("acct2", "Other")
			f.SeedTunnel("acct2", "pco-abc", nil)
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "a name that needs escaping", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, oddName, nil)
			return findTunnel(acct, oddName)
		}),
		listed("FindTunnel", "in an account that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(oddAccount, "Odd")
			f.SeedTunnel(oddAccount, "pco-abc", nil)
			return findTunnel(oddAccount, "pco-abc")
		}),
		refused("FindTunnel", "two of that name", isError, func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "pco-abc", nil)
			f.SeedTunnel(acct, "pco-abc", nil)
			return findTunnel(acct, "pco-abc")
		}).listing(),
		listed("FindTunnel", "a deleted tunnel of the same name before a live one", func(f *cffake.Fake) call {
			std(f)
			f.SeedDeletedTunnel(acct, "pco-abc")
			f.SeedTunnel(acct, "pco-abc", nil)
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "a deleted tunnel of the same name after a live one", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "pco-abc", nil)
			f.SeedDeletedTunnel(acct, "pco-abc")
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "several deleted ones next to a live one", func(f *cffake.Fake) call {
			std(f)
			for range 3 {
				f.SeedDeletedTunnel(acct, "pco-abc")
			}
			f.SeedTunnel(acct, "pco-abc", nil)
			f.SeedDeletedTunnel(acct, "pco-abc")
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "only a deleted one", func(f *cffake.Fake) call {
			std(f)
			f.SeedDeletedTunnel(acct, "pco-abc")
			return findTunnel(acct, "pco-abc")
		}),
		listed("FindTunnel", "a deleted one among many", func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "other-", 120)
			f.SeedDeletedTunnel(acct, "pco-abc")
			return findTunnel(acct, "pco-abc")
		}),
		refused("FindTunnel", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return findTunnel("acct9", "pco-abc")
		}),
		refused("FindTunnel", "blank name", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return findTunnel(acct, " ")
		}),
		refused("FindTunnel", "blank account", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return findTunnel("", "pco-abc")
		}),
	}
}

func tunnelsScenarios() []scenario {
	among := func(probes int, prefix string) seedFunc {
		return func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "other-", 120)
			seedTunnels(f, acct, "pco-abc_probe_", probes)
			return tunnels(acct, prefix)
		}
	}
	return []scenario{
		listed("Tunnels", "two among 120 others", among(2, "pco-abc_probe_")).injecting("tunnel.read"),
		listed("Tunnels", "none among 120 others", among(0, "pco-abc_probe_")),
		listed("Tunnels", "49, one short page", among(49, "pco-abc_probe_")),
		listed("Tunnels", "50, one full page and an empty one", among(50, "pco-abc_probe_")),
		listed("Tunnels", "100, two full pages and an empty one", among(100, "pco-abc_probe_")),
		listed("Tunnels", "120, three pages", among(120, "pco-abc_probe_")),
		listed("Tunnels", "a short prefix", among(30, "pco-")),
		listed("Tunnels", "the others, over three pages", among(5, "other-")),
		listed("Tunnels", "a prefix that matches nothing", among(5, "nothing-")),
		listed("Tunnels", "the prefix is case sensitive", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "PCO-abc", nil)
			f.SeedTunnel(acct, "pco-abc", nil)
			return tunnels(acct, "pco-")
		}),
		listed("Tunnels", "a prefix that needs escaping", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "a&b+c d%e é1", nil)
			f.SeedTunnel(acct, "a&b+c d%e é2", nil)
			f.SeedTunnel(acct, "a&b", nil)
			return tunnels(acct, "a&b+c d%e é")
		}),
		listed("Tunnels", "other accounts stay out", func(f *cffake.Fake) call {
			std(f)
			f.AddAccount("acct2", "Other")
			seedTunnels(f, "acct2", "pco-abc_probe_", 70)
			f.SeedTunnel(acct, "pco-abc_probe_x", nil)
			return tunnels(acct, "pco-abc_probe_")
		}),
		listed("Tunnels", "deleted ones among the live ones", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "pco-abc_probe_1", nil)
			f.SeedDeletedTunnel(acct, "pco-abc_probe_2")
			f.SeedTunnel(acct, "pco-abc_probe_3", nil)
			f.SeedDeletedTunnel(acct, "pco-abc_probe_4")
			f.SeedDeletedTunnel(acct, "other-1")
			return tunnels(acct, "pco-abc_probe_")
		}),
		listed("Tunnels", "deleted ones that fill the first pages", func(f *cffake.Fake) call {
			std(f)
			for i := range 120 {
				f.SeedDeletedTunnel(acct, fmt.Sprintf("pco-abc_probe_d%03d", i))
			}
			seedTunnels(f, acct, "pco-abc_probe_l", 5)
			return tunnels(acct, "pco-abc_probe_")
		}),
		listed("Tunnels", "only deleted ones", func(f *cffake.Fake) call {
			std(f)
			for i := range 60 {
				f.SeedDeletedTunnel(acct, fmt.Sprintf("pco-abc_probe_d%03d", i))
			}
			return tunnels(acct, "pco-abc_probe_")
		}),
		listed("Tunnels", "live and deleted ones past a full page", func(f *cffake.Fake) call {
			std(f)
			for i := range 25 {
				f.SeedTunnel(acct, fmt.Sprintf("pco-abc_probe_a%03d", i), nil)
				f.SeedDeletedTunnel(acct, fmt.Sprintf("pco-abc_probe_b%03d", i))
			}
			return tunnels(acct, "pco-abc_probe_")
		}),
		refused("Tunnels", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return tunnels("acct9", "pco-")
		}),
		refused("Tunnels", "blank prefix", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return tunnels(acct, " ")
		}),
	}
}

func createTunnelScenarios() []scenario {
	return []scenario{
		ok("CreateTunnel", "new name", func(f *cffake.Fake) call {
			std(f)
			return createTunnel(acct, "pco-abc")
		}).injecting("tunnel.write"),
		ok("CreateTunnel", "a name that needs escaping", func(f *cffake.Fake) call {
			std(f)
			return createTunnel(acct, "pco a&b+c=d%e/f é?")
		}),
		ok("CreateTunnel", "the name of a tunnel in another account", func(f *cffake.Fake) call {
			std(f)
			f.AddAccount("acct2", "Other")
			f.SeedTunnel("acct2", "pco-abc", nil)
			return createTunnel(acct, "pco-abc")
		}),
		ok("CreateTunnel", "in an account that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(oddAccount, "Odd")
			return createTunnel(oddAccount, "pco-abc")
		}),
		ok("CreateTunnel", "the name of a deleted tunnel", func(f *cffake.Fake) call {
			std(f)
			f.SeedDeletedTunnel(acct, "pco-abc")
			return createTunnel(acct, "pco-abc")
		}),
		ok("CreateTunnel", "a name that was used and deleted twice", func(f *cffake.Fake) call {
			std(f)
			f.SeedDeletedTunnel(acct, "pco-abc")
			f.SeedDeletedTunnel(acct, "pco-abc")
			return createTunnel(acct, "pco-abc")
		}),
		refused("CreateTunnel", "name taken next to a deleted one", cfapi.IsConflict, func(f *cffake.Fake) call {
			std(f)
			f.SeedDeletedTunnel(acct, "pco-abc")
			f.SeedTunnel(acct, "pco-abc", nil)
			return createTunnel(acct, "pco-abc")
		}),
		refused("CreateTunnel", "name taken", cfapi.IsConflict, func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "pco-abc", nil)
			return createTunnel(acct, "pco-abc")
		}),
		refused("CreateTunnel", "name taken among many", cfapi.IsConflict, func(f *cffake.Fake) call {
			std(f)
			seedTunnels(f, acct, "other-", 120)
			f.SeedTunnel(acct, "pco-abc", nil)
			return createTunnel(acct, "pco-abc")
		}),
		refused("CreateTunnel", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return createTunnel("acct9", "pco-abc")
		}),
		refused("CreateTunnel", "blank name", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return createTunnel(acct, "")
		}),
		listed("CreateTunnel", "a tunnel from creation to deletion and again", func(f *cffake.Fake) call {
			std(f)
			return func(a cfapi.API) (any, error) {
				created, err := a.CreateTunnel(ctx, acct, "pco-abc")
				if err != nil {
					return nil, err
				}
				found, exists, err := a.FindTunnel(ctx, acct, "pco-abc")
				if err != nil {
					return nil, err
				}
				cfg, err := a.TunnelConfig(ctx, acct, created.ID)
				if err != nil {
					return nil, err
				}
				version, err := a.PutTunnelConfig(ctx, acct, created.ID, plannerRules())
				if err != nil {
					return nil, err
				}
				runToken, err := a.TunnelToken(ctx, acct, created.ID)
				if err != nil {
					return nil, err
				}
				if err := a.DeleteTunnel(ctx, acct, created.ID); err != nil {
					return nil, err
				}
				_, stillThere, err := a.FindTunnel(ctx, acct, "pco-abc")
				if err != nil {
					return nil, err
				}
				again, err := a.CreateTunnel(ctx, acct, "pco-abc")
				if err != nil {
					return nil, err
				}
				live, err := a.Tunnels(ctx, acct, "pco-")
				return []any{created, found, exists, cfg, version, runToken, stillThere, again, live}, err
			}
		}),
	}
}

func deleteTunnelScenarios() []scenario {
	return []scenario{
		ok("DeleteTunnel", "a tunnel without connectors", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "other", nil)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return deleteTunnel(acct, tun.ID)
		}).injecting("tunnel.write"),
		ok("DeleteTunnel", "in an account that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(oddAccount, "Odd")
			tun := f.SeedTunnel(oddAccount, "pco-abc", nil)
			return deleteTunnel(oddAccount, tun.ID)
		}),
		refused("DeleteTunnel", "a tunnel with connectors", isStatus(http.StatusBadRequest), func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 1, Connections: 4}})
			return deleteTunnel(acct, tun.ID)
		}),
		refused("DeleteTunnel", "a tunnel that was deleted already", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return deleteTunnel(acct, tun.ID)
		}),
		refused("DeleteTunnel", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return deleteTunnel(acct, "no-such-tunnel")
		}),
		refused("DeleteTunnel", "the tunnel of another account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			f.AddAccount("acct2", "Other")
			tun := f.SeedTunnel("acct2", "pco-abc", nil)
			return deleteTunnel(acct, tun.ID)
		}),
		refused("DeleteTunnel", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return deleteTunnel("acct9", "no-such-tunnel")
		}),
		refused("DeleteTunnel", "an id that cannot be part of a path", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return deleteTunnel(acct, "a/b")
		}),
	}
}

func tokenScenarios() []scenario {
	return []scenario{
		ok("TunnelToken", "a tunnel", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "first", nil)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return tunnelToken(acct, tun.ID)
		}).injecting("tunnel.read"),
		refused("TunnelToken", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return tunnelToken(acct, tun.ID)
		}),
		refused("TunnelToken", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return tunnelToken(acct, "no-such-tunnel")
		}),
		refused("TunnelToken", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return tunnelToken("acct9", "no-such-tunnel")
		}),
		refused("TunnelToken", "blank tunnel", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return tunnelToken(acct, "")
		}),
	}
}

// newSecret is a tunnel secret of the length Cloudflare asks for.
var newSecret = []byte("new-secret-of-thirty-two-bytes!!")

func rotateScenarios() []scenario {
	return []scenario{
		ok("RotateTunnelSecret", "a tunnel", func(f *cffake.Fake) call {
			std(f)
			f.SeedTunnel(acct, "other", nil)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return rotateSecret(acct, tun.ID, newSecret)
		}).injecting("tunnel.write"),
		ok("RotateTunnelSecret", "a tunnel with connectors, which keep their connections", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 1, Connections: 4}})
			return rotateSecret(acct, tun.ID, newSecret)
		}),
		ok("RotateTunnelSecret", "twice", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return func(a cfapi.API) (any, error) {
				if err := a.RotateTunnelSecret(ctx, acct, tun.ID, newSecret); err != nil {
					return nil, err
				}
				return nil, a.RotateTunnelSecret(ctx, acct, tun.ID, []byte("and-another-one-of-thirty-two-by"))
			}
		}),
		refused("RotateTunnelSecret", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return rotateSecret(acct, tun.ID, newSecret)
		}),
		refused("RotateTunnelSecret", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return rotateSecret("acct9", "no-such-tunnel", newSecret)
		}),
		refused("RotateTunnelSecret", "a short secret", isInvalid, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return rotateSecret(acct, tun.ID, newSecret[:31])
		}),
		refused("RotateTunnelSecret", "an id that cannot be part of a path", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return rotateSecret(acct, "a/b", newSecret)
		}),
	}
}

func cleanUpScenarios() []scenario {
	return []scenario{
		ok("CleanUpConnections", "a tunnel with connectors", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			other := f.SeedTunnel(acct, "other", nil)
			f.SetConnectors(acct, tun.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 1, Connections: 4}, {ID: "c2", Connections: 1}})
			f.SetConnectors(acct, other.ID, []cfapi.Connector{{ID: "c3", Version: "2026.9.0", ConfigVersion: 1, Connections: 2}})
			return cleanUp(tun.ID)
		}).injecting("tunnel.write"),
		ok("CleanUpConnections", "a tunnel without connectors", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return cleanUp(tun.ID)
		}),
		refused("CleanUpConnections", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return cleanUp(tun.ID)
		}),
		refused("CleanUpConnections", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return cleanUp("no-such-tunnel")
		}),
		refused("CleanUpConnections", "blank tunnel", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return cleanUp("")
		}),
	}
}

// plannerRules has every kind of rule the planner emits for a tunnel: hosts of
// either scheme with each of the origin options, a wildcard, a held host, the
// sentinel and the catch-all.
func plannerRules() []planner.IngressRule {
	return []planner.IngressRule{
		{Hostname: "app.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "hosted.example.com", Service: "http://10.0.0.5:8081", HTTPHostHeader: "internal.lan"},
		{Hostname: "secure.example.com", Service: "https://10.0.0.5:8443", OriginServerName: "secure.example.com"},
		{Hostname: "*.wild.example.com", Service: "https://10.0.0.6:8443", MatchSNIToHost: true},
		{Hostname: "insecure.example.com", Service: "https://[fd00::7]:8443", NoTLSVerify: true},
		{Hostname: "sni.example.com", Service: "https://10.0.0.8:8443", OriginServerName: "sni.lan", HTTPHostHeader: "sni.example.com"},
		{
			Hostname: "all.example.com", Service: "https://10.0.0.9:8443",
			OriginServerName: "sni.lan", MatchSNIToHost: true, NoTLSVerify: true, HTTPHostHeader: "internal.lan",
		},
		{Hostname: "held.example.com", Service: "http_status:503"},
		{Hostname: "ünï.example.com", Service: `http://10.0.0.10:80/a b?c=d&e="f"`},
		planner.SentinelRule(planner.Writer{InstallID: "abc", Generation: 3, Nonce: "n0nce"}),
		planner.CatchAllRule(),
	}
}

func configScenarios() []scenario {
	return []scenario{
		ok("TunnelConfig", "a tunnel without a configuration", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return tunnelConfig(acct, tun.ID)
		}).injecting("tunnel.read"),
		ok("TunnelConfig", "every ingress option", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", plannerRules())
			return tunnelConfig(acct, tun.ID)
		}),
		ok("TunnelConfig", "a catch-all only", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", []planner.IngressRule{planner.CatchAllRule()})
			return tunnelConfig(acct, tun.ID)
		}),
		ok("TunnelConfig", "settings pco does not manage", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", plannerRules())
			f.SetForeign(acct, tun.ID, true)
			return tunnelConfig(acct, tun.ID)
		}),
		ok("TunnelConfig", "settings pco does not manage and no rules", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetForeign(acct, tun.ID, true)
			return tunnelConfig(acct, tun.ID)
		}),
		ok("TunnelConfig", "a hand made configuration without a catch-all", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}})
			return tunnelConfig(acct, tun.ID)
		}),
		ok("TunnelConfig", "a hundred rules", func(f *cffake.Fake) call {
			std(f)
			rules := make([]planner.IngressRule, 0, 101)
			for i := range 100 {
				rules = append(rules, planner.IngressRule{Hostname: fmt.Sprintf("h%03d.example.com", i), Service: "http://10.0.0.5:80"})
			}
			tun := f.SeedTunnel(acct, "pco-abc", append(rules, planner.CatchAllRule()))
			return tunnelConfig(acct, tun.ID)
		}),
		refused("TunnelConfig", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return tunnelConfig(acct, tun.ID)
		}),
		refused("TunnelConfig", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return tunnelConfig(acct, "no-such-tunnel")
		}),
		refused("TunnelConfig", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return tunnelConfig("acct9", "no-such-tunnel")
		}),
	}
}

func putConfigScenarios() []scenario {
	put := func(prepare func(f *cffake.Fake, id string), rules []planner.IngressRule) seedFunc {
		return func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			prepare(f, tun.ID)
			return putTunnelConfig(acct, tun.ID, rules)
		}
	}
	none := func(*cffake.Fake, string) {}
	catchAll := []planner.IngressRule{planner.CatchAllRule()}
	return []scenario{
		ok("PutTunnelConfig", "a catch-all", put(none, catchAll)).injecting("tunnel.write"),
		ok("PutTunnelConfig", "every ingress option", put(none, plannerRules())),
		ok("PutTunnelConfig", "over a configuration that was there", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", []planner.IngressRule{{Hostname: "old.example.com", Service: "http://10.0.0.1:80"}, planner.CatchAllRule()})
			return putTunnelConfig(acct, tun.ID, plannerRules())
		}),
		ok("PutTunnelConfig", "settings pco does not manage are dropped", put(func(f *cffake.Fake, id string) {
			f.SetForeign(acct, id, true)
		}, plannerRules())),
		ok("PutTunnelConfig", "the same rules again count a new version", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return func(a cfapi.API) (any, error) {
				var versions []int
				for range 3 {
					v, err := a.PutTunnelConfig(ctx, acct, tun.ID, plannerRules())
					if err != nil {
						return versions, err
					}
					versions = append(versions, v)
				}
				return versions, nil
			}
		}),
		ok("PutTunnelConfig", "written, then read back", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetForeign(acct, tun.ID, true)
			return func(a cfapi.API) (any, error) {
				before, err := a.TunnelConfig(ctx, acct, tun.ID)
				if err != nil {
					return nil, err
				}
				version, err := a.PutTunnelConfig(ctx, acct, tun.ID, plannerRules())
				if err != nil {
					return nil, err
				}
				after, err := a.TunnelConfig(ctx, acct, tun.ID)
				return []any{before, version, after}, err
			}
		}),
		refused("PutTunnelConfig", "a hostname on the last rule", isStatus(http.StatusBadRequest),
			put(none, []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}})),
		refused("PutTunnelConfig", "a rule that matches everything before the last", isStatus(http.StatusBadRequest),
			put(none, []planner.IngressRule{planner.CatchAllRule(), planner.CatchAllRule()})),
		refused("PutTunnelConfig", "a rule without a service", isStatus(http.StatusBadRequest),
			put(none, []planner.IngressRule{{Hostname: "a.example.com"}, planner.CatchAllRule()})),
		refused("PutTunnelConfig", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return putTunnelConfig(acct, tun.ID, catchAll)
		}),
		refused("PutTunnelConfig", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return putTunnelConfig(acct, "no-such-tunnel", catchAll)
		}),
		refused("PutTunnelConfig", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return putTunnelConfig("acct9", "no-such-tunnel", catchAll)
		}),
	}
}

func connectorScenarios() []scenario {
	return []scenario{
		ok("Connectors", "none", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			return connectors(acct, tun.ID)
		}).injecting("tunnel.read"),
		ok("Connectors", "several", func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedTunnel(acct, "pco-abc", nil)
			f.SetConnectors(acct, tun.ID, []cfapi.Connector{
				{ID: "c1", Version: "2026.9.0", ConfigVersion: 4, Connections: 4, OriginIP: "203.0.113.10"},
				{ID: "c2", Version: "2026.8.1", ConfigVersion: 3, Connections: 1, OriginIP: "2001:db8::7"},
				{ID: "c3", Version: "2026.8.1"},
			})
			return connectors(acct, tun.ID)
		}),
		ok("Connectors", "of the tunnel asked for", func(f *cffake.Fake) call {
			std(f)
			first := f.SeedTunnel(acct, "first", nil)
			second := f.SeedTunnel(acct, "second", nil)
			f.SetConnectors(acct, first.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", Connections: 2}})
			return connectors(acct, second.ID)
		}),
		ok("Connectors", "in an account that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(oddAccount, "Odd")
			tun := f.SeedTunnel(oddAccount, "pco-abc", nil)
			f.SetConnectors(oddAccount, tun.ID, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 1, Connections: 2}})
			return connectors(oddAccount, tun.ID)
		}),
		refused("Connectors", "a deleted tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			tun := f.SeedDeletedTunnel(acct, "pco-abc")
			return connectors(acct, tun.ID)
		}),
		refused("Connectors", "unknown tunnel", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return connectors(acct, "no-such-tunnel")
		}),
		refused("Connectors", "unknown account", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return connectors("acct9", "no-such-tunnel")
		}),
	}
}

// seedZoneRecords puts records of every kind into zone1, and one into zone2.
func seedZoneRecords(f *cffake.Fake) {
	std(f)
	f.AddZone("zone2", "example.org", acct)
	f.SeedRecord(zone, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "t1.cfargotunnel.com", Proxied: true, Comment: "pco:abc app"})
	f.SeedRecord(zone, cfapi.Record{Type: "CNAME", Name: "www.example.com", Content: "app.example.com", Comment: "PCO:ABC www", TTL: 300})
	f.SeedRecord(zone, cfapi.Record{Type: "A", Name: "mail.example.com", Content: "192.0.2.10", TTL: 600})
	f.SeedRecord(zone, cfapi.Record{Type: "TXT", Name: "_pco-probe-x.example.com", Content: `"probe"`, Comment: "pco:abc probe"})
	f.SeedRecord(zone, cfapi.Record{Type: "TXT", Name: "example.com", Content: `"v=spf1 -all"`})
	f.SeedRecord("zone2", cfapi.Record{Type: "CNAME", Name: "app.example.org", Content: "t1.cfargotunnel.com", Comment: "pco:abc"})
}

func recordsScenarios() []scenario {
	filtered := func(filter cfapi.RecordFilter) seedFunc {
		return func(f *cffake.Fake) call {
			seedZoneRecords(f)
			return records(zone, filter)
		}
	}
	many := func(n int, filter cfapi.RecordFilter) seedFunc {
		return func(f *cffake.Fake) call {
			std(f)
			for i := range n {
				comment := "other"
				if i%2 == 0 {
					comment = "pco:abc " + fmt.Sprint(i)
				}
				f.SeedRecord(zone, cfapi.Record{Type: "TXT", Name: fmt.Sprintf("r%03d.example.com", i), Content: "x", Comment: comment})
			}
			return records(zone, filter)
		}
	}
	return []scenario{
		ok("Records", "no filter", filtered(cfapi.RecordFilter{})).injecting("dns.read"),
		ok("Records", "by type", filtered(cfapi.RecordFilter{Type: "CNAME"})),
		ok("Records", "by type in another case", filtered(cfapi.RecordFilter{Type: "txt"})),
		ok("Records", "by name", filtered(cfapi.RecordFilter{Name: "app.example.com"})),
		ok("Records", "by name in another case", filtered(cfapi.RecordFilter{Name: "APP.Example.COM"})),
		ok("Records", "by the prefix of the comment", filtered(cfapi.RecordFilter{CommentPrefix: "pco:abc"})),
		ok("Records", "by the prefix of the comment in another case", filtered(cfapi.RecordFilter{CommentPrefix: "Pco:Abc "})),
		ok("Records", "by everything", filtered(cfapi.RecordFilter{Type: "CNAME", Name: "www.example.com", CommentPrefix: "pco:"})),
		ok("Records", "nothing matches", filtered(cfapi.RecordFilter{Type: "MX"})),
		ok("Records", "no records", func(f *cffake.Fake) call {
			std(f)
			return records(zone, cfapi.RecordFilter{})
		}),
		ok("Records", "100, one full page", many(100, cfapi.RecordFilter{})),
		ok("Records", "101, a page and one more", many(101, cfapi.RecordFilter{})),
		ok("Records", "200, two full pages", many(200, cfapi.RecordFilter{})),
		ok("Records", "250, three pages", many(250, cfapi.RecordFilter{})),
		ok("Records", "250 filtered to 125, two pages", many(250, cfapi.RecordFilter{CommentPrefix: "pco:abc"})),
		ok("Records", "250 filtered to none", many(250, cfapi.RecordFilter{CommentPrefix: "nothing"})),
		ok("Records", "of a zone that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(acct, "Acme")
			f.AddZone(oddZone, "odd.example", acct)
			f.SeedRecord(oddZone, rec("TXT", "a.odd.example", "x"))
			return records(oddZone, cfapi.RecordFilter{})
		}),
		refused("Records", "unknown zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return records("zone9", cfapi.RecordFilter{})
		}),
		refused("Records", "blank zone", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return records("", cfapi.RecordFilter{})
		}),
	}
}

func createRecordScenarios() []scenario {
	create := func(r cfapi.Record) seedFunc {
		return func(f *cffake.Fake) call {
			seedZoneRecords(f)
			return createRecord(zone, r)
		}
	}
	return []scenario{
		ok("CreateRecord", "a proxied CNAME", create(cname("new.example.com", "t1.cfargotunnel.com"))).injecting("dns.write"),
		ok("CreateRecord", "an A record with a TTL", create(cfapi.Record{Type: "A", Name: "host.example.com", Content: "192.0.2.77", TTL: 300})),
		ok("CreateRecord", "an A record with no TTL", create(rec("A", "host.example.com", "192.0.2.77"))),
		ok("CreateRecord", "the least TTL", create(cfapi.Record{Type: "A", Name: "host.example.com", Content: "192.0.2.77", TTL: 30})),
		ok("CreateRecord", "the greatest TTL", create(cfapi.Record{Type: "A", Name: "host.example.com", Content: "192.0.2.77", TTL: 86400})),
		refused("CreateRecord", "a TTL below the least", isStatus(http.StatusBadRequest), create(cfapi.Record{
			Type: "A", Name: "host.example.com", Content: "192.0.2.77", TTL: 29,
		})),
		refused("CreateRecord", "a TTL above the greatest", isStatus(http.StatusBadRequest), create(cfapi.Record{
			Type: "A", Name: "host.example.com", Content: "192.0.2.77", TTL: 86401,
		})),
		ok("CreateRecord", "a proxied record has the automatic TTL", create(cfapi.Record{
			Type: "A", Name: "host.example.com", Content: "192.0.2.77", Proxied: true, TTL: 300,
		})),
		ok("CreateRecord", "a TXT record with quotes and a comment", create(cfapi.Record{
			Type: "TXT", Name: "_pco-probe-y.example.com", Content: `"a \"quoted\" é"`, Comment: "pco:abc probe é",
		})),
		ok("CreateRecord", "a name that is not inside the zone", create(rec("A", "host", "192.0.2.77"))),
		ok("CreateRecord", "a name in capitals", create(rec("A", "HOST.Example.COM.", "192.0.2.77"))),
		ok("CreateRecord", "the apex written as @", create(rec("TXT", "@", "x"))),
		ok("CreateRecord", "a CNAME at the apex next to a TXT record", create(rec("CNAME", "example.com", "app.example.com"))),
		ok("CreateRecord", "a proxied CNAME next to a TXT record", create(cname("_pco-probe-x.example.com", "t1.cfargotunnel.com"))),
		refused("CreateRecord", "a CNAME that is not proxied next to a TXT record", cfapi.IsConflict,
			create(rec("CNAME", "_pco-probe-x.example.com", "t1.cfargotunnel.com"))),
		ok("CreateRecord", "the ID and time of the record given are ignored", create(cfapi.Record{
			ID: "mine", ModifiedOn: time.Unix(1, 0), Type: "A", Name: "host.example.com", Content: "192.0.2.77",
		})),
		ok("CreateRecord", "in a zone that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(acct, "Acme")
			f.AddZone(oddZone, "odd.example", acct)
			return createRecord(oddZone, rec("A", "a.odd.example", "192.0.2.1"))
		}),
		refused("CreateRecord", "an identical A record", cfapi.IsConflict, create(rec("A", "mail.example.com", "192.0.2.10"))),
		refused("CreateRecord", "an identical TXT record", cfapi.IsConflict, create(rec("TXT", "example.com", `"v=spf1 -all"`))),
		refused("CreateRecord", "a CNAME next to another record", cfapi.IsConflict, create(cname("mail.example.com", "x.example.com"))),
		refused("CreateRecord", "an A record next to a CNAME", cfapi.IsConflict, create(rec("A", "www.example.com", "192.0.2.5"))),
		refused("CreateRecord", "a CNAME next to a CNAME", cfapi.IsConflict, create(cname("APP.example.com", "other.example.com"))),
		refused("CreateRecord", "a CNAME at the apex next to an A record", cfapi.IsConflict, func(f *cffake.Fake) call {
			seedZoneRecords(f)
			f.SeedRecord(zone, rec("A", "example.com", "192.0.2.1"))
			return createRecord(zone, rec("CNAME", "example.com", "app.example.com"))
		}),
		refused("CreateRecord", "unknown zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return createRecord("zone9", rec("A", "a.example.com", "192.0.2.1"))
		}),
		refused("CreateRecord", "no name", isInvalid, create(rec("A", "", "192.0.2.1"))),
		refused("CreateRecord", "no type", isInvalid, create(rec("", "a.example.com", "192.0.2.1"))),
	}
}

func updateRecordScenarios() []scenario {
	// update runs UpdateRecord on the record "target", an A record that sits
	// among the ones of seedZoneRecords.
	update := func(change func(r *cfapi.Record)) seedFunc {
		return func(f *cffake.Fake) call {
			seedZoneRecords(f)
			r := f.SeedRecord(zone, cfapi.Record{ID: "target", Type: "A", Name: "target.example.com", Content: "192.0.2.50", Comment: "pco:abc t"})
			change(&r)
			return updateRecord(zone, r)
		}
	}
	return []scenario{
		ok("UpdateRecord", "new content", update(func(r *cfapi.Record) { r.Content = "192.0.2.51" })).injecting("dns.write"),
		ok("UpdateRecord", "changed to proxied", update(func(r *cfapi.Record) { r.Proxied, r.TTL = true, 300 })),
		ok("UpdateRecord", "changed to another type", update(func(r *cfapi.Record) { r.Type, r.Content = "TXT", `"text"` })),
		ok("UpdateRecord", "comment dropped", update(func(r *cfapi.Record) { r.Comment = "" })),
		ok("UpdateRecord", "a TTL", update(func(r *cfapi.Record) { r.TTL = 900 })),
		refused("UpdateRecord", "a TTL below the least", isStatus(http.StatusBadRequest), update(func(r *cfapi.Record) { r.TTL = 29 })),
		refused("UpdateRecord", "a TTL above the greatest", isStatus(http.StatusBadRequest), update(func(r *cfapi.Record) { r.TTL = 86401 })),
		ok("UpdateRecord", "renamed in capitals", update(func(r *cfapi.Record) { r.Name = "RENAMED" })),
		ok("UpdateRecord", "not changed", update(func(*cfapi.Record) {})),
		ok("UpdateRecord", "a record whose id needs escaping", func(f *cffake.Fake) call {
			std(f)
			r := f.SeedRecord(zone, cfapi.Record{ID: "r?1 #x%2F é", Type: "A", Name: "a.example.com", Content: "192.0.2.1"})
			r.Content = "192.0.2.2"
			return updateRecord(zone, r)
		}),
		refused("UpdateRecord", "to the name of a CNAME", cfapi.IsConflict, update(func(r *cfapi.Record) { r.Name = "app.example.com" })),
		refused("UpdateRecord", "to a CNAME next to another record", cfapi.IsConflict, update(func(r *cfapi.Record) {
			r.Type, r.Name, r.Content = "CNAME", "mail.example.com", "x.example.com"
		})),
		refused("UpdateRecord", "to an identical record", isStatus(http.StatusBadRequest), update(func(r *cfapi.Record) {
			r.Name, r.Content = "mail.example.com", "192.0.2.10"
		})),
		refused("UpdateRecord", "unknown record", cfapi.IsNotFound, func(f *cffake.Fake) call {
			seedZoneRecords(f)
			r := rec("A", "a.example.com", "192.0.2.1")
			r.ID = "no-such-record"
			return updateRecord(zone, r)
		}),
		refused("UpdateRecord", "the record of another zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			seedZoneRecords(f)
			other := f.SeedRecord("zone2", cfapi.Record{ID: "elsewhere", Type: "A", Name: "a.example.org", Content: "192.0.2.1"})
			return updateRecord(zone, other)
		}),
		refused("UpdateRecord", "unknown zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			r := rec("A", "a.example.com", "192.0.2.1")
			r.ID = "r1"
			return updateRecord("zone9", r)
		}),
		refused("UpdateRecord", "no id", isInvalid, update(func(r *cfapi.Record) { r.ID = "" })),
		refused("UpdateRecord", "no name", isInvalid, update(func(r *cfapi.Record) { r.Name = "" })),
	}
}

func deleteRecordScenarios() []scenario {
	return []scenario{
		ok("DeleteRecord", "a record", func(f *cffake.Fake) call {
			seedZoneRecords(f)
			r := f.SeedRecord(zone, rec("A", "gone.example.com", "192.0.2.1"))
			return deleteRecord(zone, r.ID)
		}).injecting("dns.write"),
		ok("DeleteRecord", "a record whose id needs escaping", func(f *cffake.Fake) call {
			std(f)
			f.SeedRecord(zone, cfapi.Record{ID: "r?1 #x%2F é", Type: "A", Name: "a.example.com", Content: "192.0.2.1"})
			return deleteRecord(zone, "r?1 #x%2F é")
		}),
		ok("DeleteRecord", "from a zone that needs escaping", func(f *cffake.Fake) call {
			f.AddAccount(acct, "Acme")
			f.AddZone(oddZone, "odd.example", acct)
			r := f.SeedRecord(oddZone, rec("A", "a.odd.example", "192.0.2.1"))
			return deleteRecord(oddZone, r.ID)
		}),
		refused("DeleteRecord", "unknown record", cfapi.IsNotFound, func(f *cffake.Fake) call {
			seedZoneRecords(f)
			return deleteRecord(zone, "no-such-record")
		}),
		refused("DeleteRecord", "the record of another zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			seedZoneRecords(f)
			other := f.SeedRecord("zone2", rec("A", "a.example.org", "192.0.2.1"))
			return deleteRecord(zone, other.ID)
		}),
		refused("DeleteRecord", "unknown zone", cfapi.IsNotFound, func(f *cffake.Fake) call {
			std(f)
			return deleteRecord("zone9", "r1")
		}),
		refused("DeleteRecord", "an id that cannot be part of a path", isInvalid, func(f *cffake.Fake) call {
			std(f)
			return deleteRecord(zone, "a/b")
		}),
	}
}

// TestConfigurationRoundTrip pins what the contract only compares: the rules
// that were written come back as they were, through the wire.
func TestConfigurationRoundTrip(t *testing.T) {
	f, _ := seeded(func(f *cffake.Fake) call { std(f); return nil })
	api := client(t, f)
	tun, err := api.CreateTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)

	fresh, err := api.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{}, fresh, "a tunnel nothing was written to")

	rules := plannerRules()
	version, err := api.PutTunnelConfig(ctx, acct, tun.ID, rules)
	require.NoError(t, err)
	require.Equal(t, 1, version)

	cfg, err := api.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 1, Ingress: rules}, cfg,
		"every rule, every option, and no Foreign for what Cloudflare adds itself")

	f.SetForeign(acct, tun.ID, true)
	cfg, err = api.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.True(t, cfg.Foreign)
	require.Equal(t, rules, cfg.Ingress, "the rules are still shown next to what pco does not manage")

	version, err = api.PutTunnelConfig(ctx, acct, tun.ID, nil)
	require.True(t, isInvalid(err), "nothing is written without rules: %v", err)
	require.Zero(t, version)
	cfg, err = api.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.True(t, cfg.Foreign, "a refused write changes nothing")

	version, err = api.PutTunnelConfig(ctx, acct, tun.ID, rules)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	cfg, err = api.TunnelConfig(ctx, acct, tun.ID)
	require.NoError(t, err)
	require.Equal(t, cfapi.TunnelConfig{Version: 2, Ingress: rules}, cfg, "a write replaces what pco does not manage")
}

func TestScenarioNamesAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, sc := range allScenarios() {
		key := sc.method + "/" + sc.name
		require.False(t, seen[key], "two scenarios are called %q", key)
		seen[key] = true
	}
}
