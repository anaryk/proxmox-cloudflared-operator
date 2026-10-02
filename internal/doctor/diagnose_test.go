package doctor

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const (
	www      = "www.example.com"
	tunnelID = "0a1b2c3d-0000-4000-8000-000000000001"
)

var stepNames = []string{"route", "zone", "dns", "ingress", "connector", "identity", "tcp", "http"}

// origin is a server of a test that remembers what it was asked.
type origin struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string // the Host header of every request
	snis  []string // the name a TLS client asked for
}

func (o *origin) record(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hosts = append(o.hosts, r.Host)
	if r.TLS != nil {
		o.snis = append(o.snis, r.TLS.ServerName)
	}
}

func (o *origin) requests() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.hosts...)
}

func newOrigin(t *testing.T, tls bool, h http.HandlerFunc) *origin {
	t.Helper()
	o := &origin{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.record(r)
		if h != nil {
			h(w, r)
		}
	})
	if tls {
		o.Server = httptest.NewTLSServer(handler)
	} else {
		o.Server = httptest.NewServer(handler)
	}
	t.Cleanup(o.Close)
	return o
}

func addrOf(t *testing.T, s *httptest.Server) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s.Listener.Addr().String())
	require.NoError(t, err)
	return ap
}

// servedBy is the state of a daemon that serves www.example.com, owned by
// qemu/101, on the address of srv with scheme.
func servedBy(t *testing.T, srv *httptest.Server, scheme string) engine.State {
	t.Helper()
	ap := addrOf(t, srv)
	service := scheme + "://" + ap.String()
	return engine.State{
		At: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Mode: "enforce", Complete: true, WriterVerdict: "ok",
		Routes: []engine.RouteView{{
			RouteStatus: planner.RouteStatus{Hostname: www, Owner: "qemu/101", State: planner.StateActive, Level: "port", Service: service, Zone: "example.com"},
			Guest:       &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
			Candidates:  []resolve.CandidateResult{{Addr: ap.Addr(), Source: resolve.FromStatic, OK: true}},
			Account:     "acc1",
			Rule:        &planner.IngressRule{Hostname: www, Service: service},
		}},
		Tunnels: []engine.TunnelView{{TunnelState: reconcile.TunnelState{
			AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc123", ID: tunnelID, Version: 3, Exists: true, Verified: true,
		}}},
		Connectors: []connector.Status{{TunnelID: tunnelID, Active: true, Ready: true, Connections: 4}},
	}
}

func names(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// requireFailsAt checks that the steps fail at the named one with detail, and
// that the steps after it are skipped.
func requireFailsAt(t *testing.T, steps []Step, name, detail string) {
	t.Helper()
	require.Equal(t, stepNames, names(steps))
	failed := false
	for _, s := range steps {
		switch {
		case failed:
			require.Equal(t, Step{Name: s.Name, Level: LevelWarn, Detail: "skipped"}, s)
		case s.Name == name:
			require.Equal(t, Step{Name: name, Level: LevelFail, Detail: detail}, s)
			failed = true
		default:
			require.NotEqual(t, LevelFail, s.Level, "%s fails before %s: %s", s.Name, name, s.Detail)
		}
	}
	require.True(t, failed, "no step %s", name)
}

func TestARouteThatWorksPassesEveryStep(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	service := st.Routes[0].Service

	steps, err := DiagnoseRoute(t.Context(), st, "WWW.example.com.", o.Client())

	require.NoError(t, err)
	require.Equal(t, []Step{
		{Name: "route", Level: LevelOK, Detail: "qemu/101 (web-1) holds it; state active"},
		{Name: "zone", Level: LevelOK, Detail: "zone example.com is active"},
		{Name: "dns", Level: LevelOK, Detail: "its record points at the tunnel"},
		{Name: "ingress", Level: LevelOK, Detail: "tunnel pco-abc123 sends it to " + service + " (configuration version 3)"},
		{Name: "connector", Level: LevelOK, Detail: "active, ready, 4 connections"},
		{Name: "identity", Level: LevelOK, Detail: "127.0.0.1 is the address of qemu/101, verified at identity level port (static)"},
		{Name: "tcp", Level: LevelOK, Detail: strings.TrimPrefix(service, "http://") + " answers"},
		{Name: "http", Level: LevelOK, Detail: "the origin answered 200 OK"},
	}, steps)
	require.Equal(t, []string{www}, o.requests(), "the Host header the tunnel sends")
}

func TestEachStepFailsInTurn(t *testing.T) {
	for _, tt := range []struct {
		name, step, detail string
		change             func(st *engine.State)
	}{
		{"a hostname nobody serves", "route", "qemu/101 (web-1) holds it, but nobody serves it: named in the Notes of qemu/101 but not routed; claim kept",
			func(st *engine.State) {
				r := &st.Routes[0]
				r.State, r.Reason, r.Service = planner.StateHeld, "named in the Notes of qemu/101 but not routed; claim kept", ""
				r.Rule.Service = "http_status:503"
			}},
		{"no zone", "zone", "no Cloudflare zone for this hostname in any credential", func(st *engine.State) {
			r := &st.Routes[0]
			r.State, r.Reason, r.Service, r.Zone, r.Rule, r.Account = planner.StateNoZone, "no Cloudflare zone for this hostname in any credential", "", "", nil, ""
		}},
		{"a frozen account", "zone", "account frozen: zone example.com is no longer listed by credential cred1", func(st *engine.State) {
			r := &st.Routes[0]
			r.State, r.Reason, r.Service = engine.RouteFrozen, "account frozen: zone example.com is no longer listed by credential cred1", ""
		}},
		{"a record of someone else", "dns", "a record of someone else holds the name in zone example.com: A 192.0.2.10; pco adopt www.example.com replaces it",
			func(st *engine.State) {
				st.Conflicts = []reconcile.Conflict{{Zone: "example.com", Name: "WWW.example.com", Type: "A", Content: "192.0.2.10"}}
			}},
		{"a record that lost its marker", "dns", "its record points at the tunnel but lost the marker of this install; pco adopt www.example.com takes it back",
			func(st *engine.State) { st.Lost = []string{www} }},
		{"a record not created yet", "dns", "create-record is not applied: tunnel not created yet", func(st *engine.State) {
			st.Actions = []reconcile.Action{{Kind: reconcile.CreateRecord, Target: www, Detail: "in zone example.com", Held: "tunnel not created yet"}}
		}},
		{"no tunnel", "ingress", "the tunnel of account acc1 does not exist yet", func(st *engine.State) {
			st.Tunnels[0].Exists, st.Tunnels[0].ID, st.Tunnels[0].Verified = false, "", false
		}},
		{"a tunnel in an unknown state", "ingress", "the state of the tunnel of account acc1 is not known", func(st *engine.State) {
			st.Tunnels[0].Exists, st.Tunnels[0].Unknown, st.Tunnels[0].Verified = false, true, false
		}},
		{"a held tunnel", "ingress", "tunnel pco-abc123 in account acc1 is left as it is: serves no zone pco sees", func(st *engine.State) {
			st.Tunnels[0].Held = "serves no zone pco sees"
		}},
		{"a configuration that is not verified", "ingress",
			"the configuration of tunnel pco-abc123 is not verified: the last write was held or failed (pco plan shows why)",
			func(st *engine.State) { st.Tunnels[0].Verified = false }},
		{"no rule", "ingress", "the plan has no rule for it", func(st *engine.State) { st.Routes[0].Rule = nil }},
		{"no connector", "connector", "no connector runs for tunnel pco-abc123", func(st *engine.State) { st.Connectors = nil }},
		{"a connector that is not running", "connector", "the connector of tunnel pco-abc123 is not running", func(st *engine.State) {
			st.Connectors[0].Active, st.Connectors[0].Ready = false, false
		}},
		{"a connector that is not connected", "connector", "the connector of tunnel pco-abc123 is not connected to Cloudflare", func(st *engine.State) {
			st.Connectors[0].Ready = false
		}},
		{"a withdrawn target", "identity", "identity check failed: the MAC answers on another port", func(st *engine.State) {
			r := &st.Routes[0]
			r.State, r.Reason, r.Service = planner.StateWithdrawn, "identity check failed: the MAC answers on another port", ""
			r.Rule.Service = "http_status:503"
			r.Candidates[0].OK, r.Candidates[0].Reason = false, "the MAC answers on another port"
		}},
		{"no verified address", "identity", "no verified address yet; tried 127.0.0.1 (static): ARP answered by another MAC", func(st *engine.State) {
			r := &st.Routes[0]
			r.State, r.Reason, r.Service = planner.StateUnreachable, "no verified address yet", ""
			r.Rule.Service = "http_status:503"
			r.Candidates[0].OK, r.Candidates[0].Reason = false, "ARP answered by another MAC"
		}},
		{"a target held back by the identity minimum", "identity",
			"identity level observed is below the required port; tried 127.0.0.1 (static): passed", func(st *engine.State) {
				r := &st.Routes[0]
				r.State, r.Level, r.Reason, r.Service = planner.StateUnreachable, "observed", "identity level observed is below the required port", ""
				r.Rule.Service = "http_status:503"
			}},
		{"a port that does not answer", "tcp", "dial tcp: connection refused", func(st *engine.State) {
			r := &st.Routes[0]
			r.State, r.Reason = planner.StateUnreachable, "target is not answering"
			r.Candidates[0].OK, r.Candidates[0].Reason = false, "dial tcp: connection refused"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin(t, false, nil)
			st := servedBy(t, o.Server, "http")
			tt.change(&st)

			steps, err := DiagnoseRoute(t.Context(), st, www, o.Client())

			require.NoError(t, err)
			requireFailsAt(t, steps, tt.step, tt.detail)
			require.Empty(t, o.requests(), "nothing asks the origin once a step failed")
		})
	}
}

// The identity step names the level the address was proven at, when the
// state has one.
func TestTheIdentityStepNamesTheLevel(t *testing.T) {
	for level, detail := range map[string]string{
		"observed": "127.0.0.1 is the address of qemu/101, verified at identity level observed (static)",
		"manual":   "127.0.0.1 is the address of qemu/101, verified at identity level manual (static)",
		"":         "127.0.0.1 is the address of qemu/101, verified (static)",
	} {
		o := newOrigin(t, false, nil)
		st := servedBy(t, o.Server, "http")
		st.Routes[0].Level = level

		steps, err := DiagnoseRoute(t.Context(), st, www, o.Client())

		require.NoError(t, err)
		require.Equal(t, Step{Name: "identity", Level: LevelOK, Detail: detail}, steps[5])
	}
}

// A route whose target is not verified is not published: the steps up to the
// target warn rather than fail, so that the cause is told where it is.
func TestATargetThatIsNotVerifiedIsWhereTheDiagnosisFails(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	r := &st.Routes[0]
	r.State, r.Reason, r.Service = planner.StateUnreachable, "no verified address yet", ""
	r.Rule.Service = "http_status:503"
	r.Candidates[0].OK, r.Candidates[0].Reason = false, "ARP answered by another MAC"

	steps, err := DiagnoseRoute(t.Context(), st, www, o.Client())

	require.NoError(t, err)
	require.Equal(t, Step{Name: "dns", Level: LevelWarn, Detail: "no record is published for it while its target is not verified"}, steps[2])
	require.Equal(t, Step{Name: "ingress", Level: LevelWarn, Detail: "tunnel pco-abc123 answers 503 for it until its target is verified"}, steps[3])
}

func TestWhatTheOriginAnswers(t *testing.T) {
	for _, tt := range []struct {
		name    string
		handler http.HandlerFunc
		level   Level
		detail  string
	}{
		{"a server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, LevelWarn,
			"the origin answered 502 Bad Gateway"},
		{"a page that is not there", http.NotFound, LevelOK, "the origin answered 404 Not Found"},
		{"a status of its own", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) }, LevelOK,
			"the origin answered 418 I'm a teapot"},
		{"the page nginx shows for plain HTTP on its HTTPS port", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<html><head><title>400 The plain HTTP request was sent to HTTPS port</title></head></html>")
		}, LevelFail, "origin speaks TLS: use https:// in the route"},
		{"a 400 that says something else", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "nope")
		}, LevelOK, "the origin answered 400 Bad Request"},
		{"the hint beyond what is read", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, strings.Repeat(" ", 4<<10)+"The plain HTTP request was sent to HTTPS port")
		}, LevelOK, "the origin answered 400 Bad Request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin(t, false, tt.handler)

			steps, err := DiagnoseRoute(t.Context(), servedBy(t, o.Server, "http"), www, o.Client())

			require.NoError(t, err)
			require.Equal(t, Step{Name: "http", Level: tt.level, Detail: tt.detail}, steps[7])
			require.Len(t, o.requests(), 1)
		})
	}
}

// Nothing of what the origin sends comes back but its status.
func TestTheBodyIsNotReturned(t *testing.T) {
	const marker = "body-of-the-origin"
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
		o := newOrigin(t, false, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, marker+" The plain HTTP request was sent to HTTPS port "+marker)
		})

		steps, err := DiagnoseRoute(t.Context(), servedBy(t, o.Server, "http"), www, o.Client())

		require.NoError(t, err)
		for _, s := range steps {
			require.NotContains(t, s.Detail, marker)
		}
	}
}

func TestARouteThatSaysHTTPToAnOriginThatSpeaksTLS(t *testing.T) {
	t.Run("a server that answers plain HTTP with an error page", func(t *testing.T) {
		o := newOrigin(t, true, nil)

		steps, err := DiagnoseRoute(t.Context(), servedBy(t, o.Server, "http"), www, o.Client())

		require.NoError(t, err)
		require.Equal(t, Step{Name: "http", Level: LevelFail, Detail: "origin speaks TLS: use https:// in the route"}, steps[7])
	})
	t.Run("a server that answers with a TLS alert", func(t *testing.T) {
		srv := alerting(t)

		steps, err := DiagnoseRoute(t.Context(), servedBy(t, srv, "http"), www, nil)

		require.NoError(t, err)
		require.Equal(t, Step{Name: "http", Level: LevelFail, Detail: "origin speaks TLS: use https:// in the route"}, steps[7])
	})
}

// alerting is a server that answers whatever it is sent with a TLS alert, as
// a TLS server does that is spoken to in plain HTTP.
func alerting(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(func() { _ = srv.Listener.Close() })
	go func() {
		for {
			conn, err := srv.Listener.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte{0x15, 0x03, 0x01, 0x00, 0x02, 0x02, 0x46})
			_ = conn.Close()
		}
	}()
	return srv
}

func TestTLSIsVerifiedAsTheRouteSays(t *testing.T) {
	for _, tt := range []struct {
		name   string
		rule   func(r *planner.IngressRule)
		trust  bool
		level  Level
		detail string
		sni    string
	}{
		{"a certificate of an unknown authority", func(r *planner.IngressRule) { r.OriginServerName = "example.com" }, false, LevelFail,
			"TLS verification failed (unknown authority): set sni=<name> in the route when the certificate is of another name, or no-tls-verify", "example.com"},
		{"a certificate of another name", func(r *planner.IngressRule) { r.OriginServerName = "intranet.local" }, true, LevelFail,
			"TLS verification failed (name mismatch): set sni=<name> in the route when the certificate is of another name, or no-tls-verify", ""},
		{"the name the route gives", func(r *planner.IngressRule) { r.OriginServerName = "example.com" }, true, LevelOK,
			"the origin answered 200 OK", "example.com"},
		// The certificate of the test server is for example.com and
		// *.example.com.
		{"the name of the request", func(r *planner.IngressRule) { r.MatchSNIToHost = true }, true, LevelOK,
			"the origin answered 200 OK", www},
		{"no verification", func(r *planner.IngressRule) { r.NoTLSVerify = true }, false, LevelOK, "the origin answered 200 OK", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := newOrigin(t, true, nil)
			st := servedBy(t, o.Server, "https")
			tt.rule(st.Routes[0].Rule)
			client := &http.Client{}
			if tt.trust {
				client = o.Client()
			}

			steps, err := DiagnoseRoute(t.Context(), st, www, client)

			require.NoError(t, err)
			require.Equal(t, Step{Name: "http", Level: tt.level, Detail: tt.detail}, steps[7])
			if tt.level == LevelOK {
				o.mu.Lock()
				require.Equal(t, []string{tt.sni}, o.snis, "the name the tunnel would ask for")
				o.mu.Unlock()
			}
		})
	}
}

func TestARouteThatSaysHTTPSToAnOriginThatSpeaksPlainHTTP(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "https")
	st.Routes[0].Rule.NoTLSVerify = true

	steps, err := DiagnoseRoute(t.Context(), st, www, nil)

	require.NoError(t, err)
	require.Equal(t, Step{Name: "http", Level: LevelFail, Detail: "origin speaks plain HTTP: use http:// in the route"}, steps[7])
}

func TestTheHostHeaderIsTheOneTheTunnelSends(t *testing.T) {
	t.Run("the header the route sets", func(t *testing.T) {
		o := newOrigin(t, false, nil)
		st := servedBy(t, o.Server, "http")
		st.Routes[0].Rule.HTTPHostHeader = "intranet.local"

		_, err := DiagnoseRoute(t.Context(), st, www, nil)

		require.NoError(t, err)
		require.Equal(t, []string{"intranet.local"}, o.requests())
	})
	t.Run("a name below a wildcard", func(t *testing.T) {
		o := newOrigin(t, false, nil)
		st := servedBy(t, o.Server, "http")
		st.Routes[0].Hostname, st.Routes[0].Rule.Hostname = "*.example.com", "*.example.com"

		_, err := DiagnoseRoute(t.Context(), st, "*.example.com", nil)

		require.NoError(t, err)
		require.Equal(t, []string{"pco-diagnose.example.com"}, o.requests())
	})
}

// A redirect is told, never followed: the place it points at is not asked.
func TestARedirectIsNotFollowed(t *testing.T) {
	decoy := newOrigin(t, false, nil)
	o := newOrigin(t, false, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, decoy.URL+"/elsewhere", http.StatusFound)
	})

	steps, err := DiagnoseRoute(t.Context(), servedBy(t, o.Server, "http"), www, decoy.Client())

	require.NoError(t, err)
	require.Equal(t, Step{Name: "http", Level: LevelOK, Detail: "the origin answered 302 Found; the redirect is not followed"}, steps[7])
	require.Empty(t, decoy.requests())
}

// Only the address and port of a route the state shows verified are ever
// asked: never one the caller names, and never one the state does not vouch
// for.
func TestNothingButAVerifiedTargetIsAsked(t *testing.T) {
	t.Run("hostnames that are no route", func(t *testing.T) {
		target, decoy := newOrigin(t, false, nil), newOrigin(t, false, nil)
		st := servedBy(t, target.Server, "http")
		decoyAddr := addrOf(t, decoy.Server)
		for _, name := range []string{
			decoyAddr.String(), decoyAddr.Addr().String(), decoy.URL, "api.example.com", "www.example.com.evil.example",
			"127.0.0.1.nip.io", "", "www.example.com/../x",
		} {
			_, err := DiagnoseRoute(t.Context(), st, name, nil)

			require.Error(t, err, name)
			require.True(t, isNotFoundOrInvalid(err), "%q: %v", name, err)
		}
		require.Empty(t, target.requests())
		require.Empty(t, decoy.requests())
	})
	t.Run("a rule that points elsewhere than the route", func(t *testing.T) {
		target, decoy := newOrigin(t, false, nil), newOrigin(t, false, nil)
		st := servedBy(t, target.Server, "http")
		st.Routes[0].Rule.Service = "http://" + addrOf(t, decoy.Server).String()

		steps, err := DiagnoseRoute(t.Context(), st, www, nil)

		require.NoError(t, err)
		require.Equal(t, LevelFail, steps[7].Level)
		require.Empty(t, target.requests())
		require.Empty(t, decoy.requests())
	})
	t.Run("an address no candidate verified", func(t *testing.T) {
		target := newOrigin(t, false, nil)
		st := servedBy(t, target.Server, "http")
		st.Routes[0].Candidates[0].Addr = netip.MustParseAddr("10.9.9.9")

		steps, err := DiagnoseRoute(t.Context(), st, www, nil)

		require.NoError(t, err)
		require.Equal(t, LevelFail, steps[6].Level)
		require.Empty(t, target.requests())
	})
	t.Run("a route that lost its hostname", func(t *testing.T) {
		target := newOrigin(t, false, nil)
		st := servedBy(t, target.Server, "http")
		loser := st.Routes[0]
		loser.Owner, loser.State, loser.Reason, loser.Rule = "qemu/102", planner.StateConflict, "hostname is held by qemu/101", nil
		st.Routes[0].State, st.Routes[0].Reason, st.Routes[0].Service = planner.StateUnreachable, "no verified address yet", ""
		st.Routes[0].Rule.Service = "http_status:503"
		st.Routes = append(st.Routes, loser)

		steps, err := DiagnoseRoute(t.Context(), st, www, nil)

		require.NoError(t, err)
		require.Equal(t, "qemu/101 (web-1) holds it; state unreachable: no verified address yet", steps[0].Detail)
		require.Empty(t, target.requests())
	})
}

func isNotFoundOrInvalid(err error) bool {
	return errors.Is(err, engine.ErrNotFound) || errors.Is(err, engine.ErrInvalid)
}

func TestAnOriginThatIsGone(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	o.Close()

	steps, err := DiagnoseRoute(t.Context(), st, www, nil)

	require.NoError(t, err)
	require.Equal(t, Step{Name: "http", Level: LevelFail, Detail: "connection refused"}, steps[7])
}

// The client the diagnosis asks with: no proxy, no redirect, a deadline, and
// a connection to the target whatever the request names.
func TestTheClientOfTheDiagnosis(t *testing.T) {
	o := newOrigin(t, false, nil)
	ap := addrOf(t, o.Server)
	c := newClient(target{scheme: "http", addr: ap, host: www}, nil)

	require.Equal(t, diagnoseTimeout, c.Timeout)
	require.Equal(t, 5*time.Second, diagnoseTimeout)
	require.ErrorIs(t, c.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, tr.Proxy)
	conn, err := tr.DialContext(t.Context(), "tcp", "192.0.2.1:80")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Equal(t, ap.String(), conn.RemoteAddr().String())
}

// The target of the request is the address of the route's rule, and only a
// route the state shows verified and answering has one.
func TestOnlyAVerifiedRouteHasATarget(t *testing.T) {
	o := newOrigin(t, false, nil)
	ap := addrOf(t, o.Server)
	for _, tt := range []struct {
		name   string
		change func(r *engine.RouteView)
	}{
		{"no rule", func(r *engine.RouteView) { r.Rule = nil }},
		{"a route that is not active", func(r *engine.RouteView) { r.State = planner.StateUnreachable }},
		{"a rule that serves something else", func(r *engine.RouteView) { r.Rule.Service = "http://127.0.0.1:1" }},
		{"no candidate of the address", func(r *engine.RouteView) { r.Candidates = nil }},
		{"a candidate that failed", func(r *engine.RouteView) { r.Candidates[0].OK = false }},
		{"a candidate of another address", func(r *engine.RouteView) { r.Candidates[0].Addr = netip.MustParseAddr("10.9.9.9") }},
		{"a service that is no address", func(r *engine.RouteView) {
			r.Service, r.Rule.Service = "http://www.example.com:80", "http://www.example.com:80"
		}},
		{"a rule that answers 503", func(r *engine.RouteView) { r.Service, r.Rule.Service = "http_status:503", "http_status:503" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := servedBy(t, o.Server, "http")
			tt.change(&st.Routes[0])
			d := &diagnosis{st: st, rt: st.Routes[0], host: www}

			_, ok := d.target()

			require.False(t, ok)
		})
	}

	st := servedBy(t, o.Server, "https")
	st.Routes[0].Rule.HTTPHostHeader, st.Routes[0].Rule.OriginServerName = "intranet.local", "origin.example.com"
	d := &diagnosis{st: st, rt: st.Routes[0], host: www}
	got, ok := d.target()
	require.True(t, ok)
	require.Equal(t, target{scheme: "https", addr: ap, host: "intranet.local", sni: "origin.example.com", verify: true}, got)
}
