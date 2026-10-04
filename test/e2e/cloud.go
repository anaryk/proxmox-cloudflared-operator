//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// cloud is Cloudflare as the suite looks at it: the fake, or the real API
// with a zone of the owner's.
type cloud interface {
	// Records returns the records of the zone that f asks for. Against the
	// real API a filter that names nothing reads the whole zone, which the
	// suite never asks for.
	Records(t testing.TB, zone string, f cfapi.RecordFilter) []cfapi.Record
	// Tunnel returns the tunnel of that name, when one exists that is not
	// deleted.
	Tunnel(t testing.TB, name string) (cfapi.Tunnel, bool)
	// Ingress returns the rules of the tunnel's configuration.
	Ingress(t testing.TB, tunnelID string) []planner.IngressRule
	// RunToken returns the token a connector of the tunnel runs with.
	RunToken(t testing.TB, tunnelID string) string
	// Foreign makes a record of the zone that pco did not make.
	Foreign(t testing.TB, zone, name, content string) cfapi.Record
	// Remove deletes a record the suite made.
	Remove(t testing.TB, zone string, rec cfapi.Record)
}

// The fake's account, zone and token. The zone is not a real one, so that
// nothing the suite publishes could be mistaken for a name in use.
const (
	fakeAccount = "acct-e2e"
	fakeZoneID  = "zone-e2e"
	fakeZone    = "example.test"
	fakeToken   = "e2e-dummy-token-0123456789"

	// foreignComment is the comment of the record the suite makes itself,
	// by which it finds it again.
	foreignComment = "pco end-to-end suite"
)

// fakeCloud is the in-memory Cloudflare, served over HTTP on a loopback port
// to the daemon, setup and uninstall.
type fakeCloud struct {
	f   *cffake.Fake
	srv *fakeServer
}

func newFakeCloud() (*fakeCloud, error) {
	f := cffake.New()
	f.AddAccount(fakeAccount, "e2e")
	f.AddZone(fakeZoneID, fakeZone, fakeAccount)
	srv := &fakeServer{addr: "127.0.0.1:0", handler: cffake.Handler(f, cffake.WithToken(fakeToken))}
	if err := srv.start(); err != nil {
		return nil, err
	}
	return &fakeCloud{f: f, srv: srv}, nil
}

// url is the base URL of the API the fake serves.
func (c *fakeCloud) url() string { return "http://" + c.srv.address() + "/client/v4" }

func (c *fakeCloud) zoneID(t testing.TB, zone string) string {
	t.Helper()
	require.Equal(t, fakeZone, zone, "the fake has one zone")
	return fakeZoneID
}

func (c *fakeCloud) Records(t testing.TB, zone string, f cfapi.RecordFilter) []cfapi.Record {
	return slices.DeleteFunc(c.f.RecordsIn(c.zoneID(t, zone)), func(r cfapi.Record) bool { return !f.Matches(r) })
}

func (c *fakeCloud) Tunnel(_ testing.TB, name string) (cfapi.Tunnel, bool) {
	tunnels := c.f.TunnelsIn(fakeAccount)
	i := slices.IndexFunc(tunnels, func(tn cfapi.Tunnel) bool { return tn.Name == name })
	if i < 0 {
		return cfapi.Tunnel{}, false
	}
	return tunnels[i], true
}

func (c *fakeCloud) Ingress(t testing.TB, tunnelID string) []planner.IngressRule {
	t.Helper()
	cfg, err := c.f.TunnelConfig(context.Background(), fakeAccount, tunnelID)
	require.NoError(t, err)
	return cfg.Ingress
}

func (c *fakeCloud) RunToken(_ testing.TB, tunnelID string) string {
	return cffake.RunToken(fakeAccount, tunnelID)
}

func (c *fakeCloud) Foreign(t testing.TB, zone, name, content string) cfapi.Record {
	return c.f.SeedRecord(c.zoneID(t, zone), cfapi.Record{Type: "CNAME", Name: name, Content: content, TTL: 300, Comment: foreignComment})
}

func (c *fakeCloud) Remove(t testing.TB, zone string, rec cfapi.Record) {
	t.Helper()
	err := c.f.DeleteRecord(context.Background(), c.zoneID(t, zone), rec.ID)
	if err != nil && !cfapi.IsNotFound(err) {
		require.NoError(t, err)
	}
}

// writes returns the calls of the fake that change something, from the
// index from on. A Remove of the suite is one of them.
func (c *fakeCloud) writes(from int) []string {
	var out []string
	for _, call := range c.f.Calls()[from:] {
		switch strings.Fields(call)[0] {
		case "CreateTunnel", "DeleteTunnel", "PutTunnelConfig", "CreateRecord", "UpdateRecord", "DeleteRecord":
			out = append(out, call)
		}
	}
	return out
}

// fakeServer serves the fake on one address, and can be stopped and
// started again on it, as an outage of the API.
type fakeServer struct {
	handler http.Handler

	mu   sync.Mutex
	addr string // 127.0.0.1:0 until the first start
	srv  *http.Server
	held *heldRequest
}

// heldRequest is a request that is made to wait: the first one that match
// says yes to.
type heldRequest struct {
	match   func(*http.Request) bool
	reached chan struct{}
	release chan struct{}
}

// start serves the fake, unless it is served already.
func (s *fakeServer) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return nil
	}
	var ln net.Listener
	var err error
	// The port may take a moment to be free again after a stop.
	for range 50 {
		if ln, err = net.Listen("tcp", s.addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(s.srv)
	return nil
}

func (s *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.held
	if h != nil && h.match(r) {
		s.held = nil
	} else {
		h = nil
	}
	s.mu.Unlock()
	if h != nil {
		close(h.reached)
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
	}
	s.handler.ServeHTTP(w, r)
}

// hold makes the next request that match says yes to wait until release is
// called; reached is closed when it arrives. release also takes back a hold
// that no request met.
func (s *fakeServer) hold(match func(*http.Request) bool) (reached <-chan struct{}, release func()) {
	h := &heldRequest{match: match, reached: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	s.held = h
	s.mu.Unlock()
	var once sync.Once
	return h.reached, func() {
		once.Do(func() {
			s.mu.Lock()
			if s.held == h {
				s.held = nil
			}
			s.mu.Unlock()
			close(h.release)
		})
	}
}

func (s *fakeServer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv = nil
	}
}

func (s *fakeServer) address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// realCloud is the Cloudflare API with the owner's token and zone.
type realCloud struct {
	api     *cfapi.Client
	account string
	zones   map[string]string // id by name
}

func newRealCloud(ctx context.Context, token, zone string) (*realCloud, error) {
	api, err := cfapi.New(cfapi.Options{Token: token})
	if err != nil {
		return nil, err
	}
	zones, err := api.Zones(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(zones, func(z cfapi.Zone) bool { return strings.EqualFold(z.Name, zone) })
	if i < 0 {
		return nil, errors.New("the token does not see the zone " + zone)
	}
	return &realCloud{api: api, account: zones[i].AccountID, zones: map[string]string{zone: zones[i].ID}}, nil
}

func (c *realCloud) zoneID(t testing.TB, zone string) string {
	t.Helper()
	id, ok := c.zones[zone]
	require.True(t, ok, "zone %s", zone)
	return id
}

func (c *realCloud) Records(t testing.TB, zone string, f cfapi.RecordFilter) []cfapi.Record {
	t.Helper()
	require.NotEqual(t, cfapi.RecordFilter{}, f, "the suite does not read the whole zone")
	recs, err := c.api.Records(context.Background(), c.zoneID(t, zone), f)
	require.NoError(t, err)
	return recs
}

func (c *realCloud) Tunnel(t testing.TB, name string) (cfapi.Tunnel, bool) {
	t.Helper()
	tn, found, err := c.api.FindTunnel(context.Background(), c.account, name)
	require.NoError(t, err)
	return tn, found
}

func (c *realCloud) Ingress(t testing.TB, tunnelID string) []planner.IngressRule {
	t.Helper()
	cfg, err := c.api.TunnelConfig(context.Background(), c.account, tunnelID)
	require.NoError(t, err)
	return cfg.Ingress
}

func (c *realCloud) RunToken(t testing.TB, tunnelID string) string {
	t.Helper()
	token, err := c.api.TunnelToken(context.Background(), c.account, tunnelID)
	require.NoError(t, err)
	return token
}

func (c *realCloud) Foreign(t testing.TB, zone, name, content string) cfapi.Record {
	t.Helper()
	rec, err := c.api.CreateRecord(context.Background(), c.zoneID(t, zone),
		cfapi.Record{Type: "CNAME", Name: name, Content: content, TTL: 300, Comment: foreignComment})
	require.NoError(t, err)
	return rec
}

func (c *realCloud) Remove(t testing.TB, zone string, rec cfapi.Record) {
	t.Helper()
	err := c.api.DeleteRecord(context.Background(), c.zoneID(t, zone), rec.ID)
	if err != nil && !cfapi.IsNotFound(err) {
		require.NoError(t, err)
	}
}
