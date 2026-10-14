package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
)

var epochP = agentproto.Epoch{InstallID: pkiInstall, Generation: 1, Nonce: "n1"}

const waitFor, tick = 10 * time.Second, 5 * time.Millisecond

func pemCert(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// ctrlFake is the controller behind the endpoint: it serves one snapshot,
// which a test publishes, and records the statuses.
type ctrlFake struct {
	mu       sync.Mutex
	snap     agentproto.Snapshot
	changed  chan struct{}
	statuses []agentproto.Status
	hb       agentproto.Heartbeat
	renew    func(node string, csr []byte) ([]byte, error)
}

func newCtrlFake() *ctrlFake {
	return &ctrlFake{changed: make(chan struct{}), hb: agentproto.Heartbeat{
		Epoch: epochP, LeaseValid: true, ReadyReplicas: map[string]int{sharedID: 2},
		Nodes: map[string]agentproto.NodeFreshness{testNode: {}, "pve2": {}},
	}}
}

func (f *ctrlFake) publish(s agentproto.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *ctrlFake) Snapshot(ctx context.Context, node string, epoch agentproto.Epoch, after uint64) (agentproto.Snapshot, error) {
	for {
		f.mu.Lock()
		s, ch := f.snap, f.changed
		f.mu.Unlock()
		if s.Seq > 0 && (s.Seq > after || epoch != s.Epoch) && s.Node == node {
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return agentproto.Snapshot{}, ctx.Err()
		}
	}
}

func (f *ctrlFake) Report(_ context.Context, _ string, st agentproto.Status) (agentproto.Heartbeat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	hb := f.hb
	hb.EventAck = st.EventCursor
	return hb, nil
}

func (f *ctrlFake) Join(context.Context, pki.JoinToken, []byte, netip.Addr, clusterapi.JoinFacts) ([]byte, error) {
	return nil, errors.New("no joins here")
}

func (f *ctrlFake) Renew(_ context.Context, node string, csr []byte) ([]byte, error) {
	f.mu.Lock()
	renew := f.renew
	f.mu.Unlock()
	if renew == nil {
		return nil, errors.New("no renewal here")
	}
	return renew(node, csr)
}

func (f *ctrlFake) reported() []agentproto.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statuses)
}

// authFake takes every source and the node of every certificate, unless a
// test makes it refuse.
type authFake struct {
	mu     sync.Mutex
	refuse []agentproto.Refusal // one per check, before any is taken
	always *agentproto.Refusal
	certs  []*x509.Certificate
}

func (a *authFake) Check(cert *x509.Certificate, _ netip.Addr) (string, *agentproto.Refusal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.certs = append(a.certs, cert)
	if a.always != nil {
		r := *a.always
		return "", &r
	}
	if len(a.refuse) > 0 {
		r := a.refuse[0]
		a.refuse = a.refuse[1:]
		return "", &r
	}
	id, err := pki.IdentityOf(cert)
	if err != nil {
		return "", &agentproto.Refusal{Code: agentproto.RefusalNotEnrolled, Message: err.Error()}
	}
	return id.Node, nil
}

func (a *authFake) Allowed(netip.Addr, bool) bool { return true }

func (a *authFake) refuseAlways(r *agentproto.Refusal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.always = r
}

func (a *authFake) seen() []*x509.Certificate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.certs)
}

// ctrlServer is a controller's agent endpoint on a loopback address.
type ctrlServer struct {
	srv  *clusterapi.Server
	ctrl *ctrlFake
	auth *authFake
	addr netip.AddrPort

	mu  sync.Mutex
	cas []*x509.Certificate
}

func (s *ctrlServer) pool() *x509.CertPool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := x509.NewCertPool()
	for _, ca := range s.cas {
		p.AddCert(ca)
	}
	return p
}

func (s *ctrlServer) trust(ca *x509.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cas = append(s.cas, ca)
}

func serverCert(t *testing.T, ca pki.CA, node string) tls.Certificate {
	t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueServer(&key.PublicKey, []string{pki.NodeServerName(pkiInstall, node)}, time.Now(), rand.Reader)
	require.NoError(t, err)
	return pki.TLSCertificate(key, cert, ca.Cert)
}

// startController serves the agents at addr as node, under ca. It returns
// nil when addr cannot be listened on.
func startController(t *testing.T, ca pki.CA, node string, addr netip.AddrPort) *ctrlServer {
	t.Helper()
	s := &ctrlServer{ctrl: newCtrlFake(), auth: &authFake{}, cas: []*x509.Certificate{ca.Cert}}
	s.srv = clusterapi.New(s.ctrl, s.auth, serverCert(t, ca, node), s.pool, zerolog.Nop())
	listening := make(chan netip.AddrPort, 1)
	s.srv.OnListening(func(ap netip.AddrPort) { listening <- ap })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.srv.Serve(ctx, addr) }()
	select {
	case s.addr = <-listening:
	case <-done:
		cancel()
		return nil
	}
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return s
}

// fakeDiscover says where the controller is, as a test sets it.
type fakeDiscover struct {
	mu    sync.Mutex
	ep    Endpoint
	err   error
	calls int
}

func (d *fakeDiscover) Endpoint(context.Context) (Endpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.ep, d.err
}

func (d *fakeDiscover) set(ep Endpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ep = ep
}

func (d *fakeDiscover) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

// countingDiscover counts the questions asked of another.
type countingDiscover struct {
	Discoverer
	mu    sync.Mutex
	calls int
}

func (d *countingDiscover) Endpoint(ctx context.Context) (Endpoint, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return d.Discoverer.Endpoint(ctx)
}

func (d *countingDiscover) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// clientEnv is an agent of node pve1 of install pkiInstall, with its client.
type clientEnv struct {
	*env
	ca    pki.CA
	files TLSFiles
	log   *EventLog
	disc  Discoverer
	c     *Client
}

func newClientEnv(t *testing.T, kind pki.Kind, disc func(ce *clientEnv) Discoverer) *clientEnv {
	t.Helper()
	e := newEnv(t)
	e.clock.mu.Lock()
	e.clock.t = time.Now().UTC()
	e.clock.mu.Unlock()
	e.install, e.kind = pkiInstall, kind
	ce := &clientEnv{env: e, files: TLSFiles{Dir: filepath.Join(t.TempDir(), "tls")}, log: NewEventLog(e.clock.now)}
	d := e.deps()
	d.Event = func(ev Event) {
		e.events.add(ev)
		ce.log.Add(ev)
	}
	a, err := New(d)
	require.NoError(t, err)
	e.agent = a
	ce.ca = newCA(t, time.Now().Add(-365*day))
	require.NoError(t, e.store.SaveCA(caFiles(t, ce.ca), nil, time.Time{}))
	var re *Reissuer
	if kind == pki.KindHost {
		re = &Reissuer{Node: testNode, InstallID: pkiInstall, Store: e.store, Files: ce.files, Now: time.Now, Log: zerolog.Nop()}
	}
	ce.disc = disc(ce)
	ce.c, err = NewClient(a, ClientDeps{Discover: ce.disc, Files: ce.files, Reissue: re, Events: ce.log})
	require.NoError(t, err)
	ce.c.discoverEvery, ce.c.statusEvery, ce.c.debounce, ce.c.standDownWait = 20*time.Millisecond, 50*time.Millisecond, 5*time.Millisecond, 50*time.Millisecond
	ce.c.backoff = func(int) time.Duration { return 10 * time.Millisecond }
	return ce
}

func (ce *clientEnv) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ce.c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
		ce.agent.Wait()
	})
}

// desired is the next snapshot of the epoch the controllers here serve.
func (ce *clientEnv) desired() agentproto.Snapshot {
	s := ce.snapshot()
	s.Epoch = epochP
	return s
}

func endpointOf(s *ctrlServer, node string, ca pki.CA) Endpoint {
	return Endpoint{Node: node, Addrs: []string{s.addr.String()}, ServerName: pki.NodeServerName(pkiInstall, node), CASHA256: ca.SHA256()}
}

// oneController is an agent that follows a controller on pve2.
func oneController(t *testing.T, kind pki.Kind) (*clientEnv, *ctrlServer) {
	t.Helper()
	disc := &fakeDiscover{}
	var srv *ctrlServer
	ce := newClientEnv(t, kind, func(ce *clientEnv) Discoverer {
		srv = startController(t, ce.ca, "pve2", netip.MustParseAddrPort("127.0.0.1:0"))
		require.NotNil(t, srv)
		disc.set(endpointOf(srv, "pve2", ce.ca))
		return disc
	})
	return ce, srv
}

func TestTheClientTimesItselfAsTheProtocolSays(t *testing.T) {
	e := newEnv(t)
	c, err := NewClient(e.agent, ClientDeps{Discover: &fakeDiscover{}, Files: TLSFiles{Dir: t.TempDir()}, Reissue: &Reissuer{}})
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, c.discoverEvery, "a new holder or CA is followed within two seconds")
	require.Equal(t, 10*time.Second, c.statusEvery)
	require.Equal(t, time.Second, c.debounce)
	require.Equal(t, 30*time.Second, c.backoff(10))
	require.Equal(t, time.Second, c.backoff(0))

	for name, deps := range map[string]ClientDeps{
		"no discovery":       {Files: TLSFiles{Dir: "x"}, Reissue: &Reissuer{}},
		"no files":           {Discover: &fakeDiscover{}, Reissue: &Reissuer{}},
		"a host without one": {Discover: &fakeDiscover{}, Files: TLSFiles{Dir: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClient(e.agent, deps)
			require.Error(t, err)
		})
	}
}

func TestTheClientAppliesTheDesiredStateAndReports(t *testing.T) {
	ce, srv := oneController(t, pki.KindHost)
	srv.ctrl.publish(ce.desired())
	ce.run(t)

	require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 1 }, waitFor, tick)
	require.Eventually(t, func() bool {
		for _, st := range srv.ctrl.reported() {
			if st.SeqApplied == 1 && st.Node == testNode && st.Kind == pki.KindHost && st.EpochSeen == epochP {
				return true
			}
		}
		return false
	}, waitFor, tick)
	require.Eventually(t, func() bool { return ce.c.View().ReadyReplicas[sharedID] == 2 }, waitFor, tick, "the heartbeat reaches the agent")

	v := ce.c.View()
	require.Equal(t, "pve2", v.Controller)
	require.NotEmpty(t, v.CertFingerprint)
	require.Empty(t, v.StoodDown)
	en, found, err := ce.store.Enrolment(testNode)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, v.CertFingerprint, en.SHA256, "the certificate the node dials with is its enrolment")
	require.Equal(t, v.CertFingerprint, pki.Fingerprint(srv.auth.seen()[0]))

	t.Run("a new seq comes through the long-poll", func(t *testing.T) {
		srv.ctrl.publish(ce.desired())
		require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 2 }, waitFor, tick)
	})
}

// A status the controller refuses as too large is said in the view, so that
// an admin sees why the controller hears nothing of the node, until a status
// goes through.
func TestAStatusRefusedAsTooLargeIsSaid(t *testing.T) {
	ce, srv := oneController(t, pki.KindHost)
	srv.ctrl.publish(ce.desired())
	ce.c.statusEvery = time.Hour // only the triggers below send a status
	ce.run(t)
	// The first status goes at once and the snapshot taken triggers one
	// more; once both arrived, no status is due until the next trigger, so
	// none goes while the events are added and none clears what is said.
	require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 1 && len(srv.ctrl.reported()) >= 2 }, waitFor, tick)
	big := strings.Repeat("x", 16*1024)
	for range 80 {
		ce.log.Add(Event{At: time.Now(), Level: levelInfo, Kind: kindNode, Subject: testNode, Message: big})
	}
	refused := func(p string) bool {
		return strings.HasPrefix(p, "the controller refused the last status as too large (80 events")
	}

	ce.c.Trigger()

	require.Eventually(t, func() bool { return slices.ContainsFunc(ce.c.View().Problems, refused) }, waitFor, tick)

	t.Run("and no more once a status goes through", func(t *testing.T) {
		ce.c.Trigger()
		require.Eventually(t, func() bool { return !slices.ContainsFunc(ce.c.View().Problems, refused) }, waitFor, tick)
		require.NotEmpty(t, srv.ctrl.reported())
	})
}

func TestADefinitiveRefusalStandsTheAgentDownOnce(t *testing.T) {
	ce, srv := oneController(t, pki.KindHost)
	srv.ctrl.publish(ce.desired())
	ce.run(t)
	require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 1 }, waitFor, tick)

	srv.auth.refuseAlways(&agentproto.Refusal{Code: agentproto.RefusalRemoved, Message: "node pve1 was removed"})
	require.Eventually(t, func() bool { return ce.c.View().StoodDown != "" }, waitFor, tick)
	require.Contains(t, ce.c.View().StoodDown, "this node was removed from install "+pkiInstall)
	require.Empty(t, ce.EgressSetOf(), "the egress set is emptied")

	// It asks again after a while, and does not stand down a second time.
	asked := len(srv.auth.seen())
	require.Eventually(t, func() bool { return len(srv.auth.seen()) >= asked+3 }, waitFor, tick)
	stood := 0
	for _, m := range ce.events.messages() {
		if strings.Contains(m, "was removed from install") {
			stood++
		}
	}
	require.Equal(t, 1, stood)

	t.Run("a controller that serves it again takes it back", func(t *testing.T) {
		srv.auth.refuseAlways(nil)
		srv.ctrl.publish(ce.desired())
		require.Eventually(t, func() bool { return ce.c.View().StoodDown == "" }, waitFor, tick)
	})
}

// A forgotten node's agent is not let through to the controller, which
// cannot tell it so: the registry does, through its discovery.
func TestARemovalTheDiscoveryReadsStandsTheAgentDown(t *testing.T) {
	removed := &refusedError{r: agentproto.Refusal{Code: agentproto.RefusalRemoved, Message: "the registry holds node pve1 removed"}}

	t.Run("while no controller answers", func(t *testing.T) {
		disc := &fakeDiscover{err: removed}
		ce := newClientEnv(t, pki.KindHost, func(*clientEnv) Discoverer { return disc })
		ce.apply(ce.desired())
		ce.run(t)

		require.Eventually(t, func() bool { return ce.c.View().StoodDown != "" }, waitFor, tick)
		require.Contains(t, ce.c.View().StoodDown, "this node was removed from install "+pkiInstall)
		require.Empty(t, ce.EgressSetOf())
		require.Contains(t, ce.conn.changes(), "StopAll")
	})
	t.Run("in the middle of a session", func(t *testing.T) {
		ce, srv := oneController(t, pki.KindHost)
		srv.ctrl.publish(ce.desired())
		ce.run(t)
		require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 1 }, waitFor, tick)

		ce.disc.(*fakeDiscover).fail(removed)

		require.Eventually(t, func() bool { return ce.c.View().StoodDown != "" }, waitFor, tick)
		require.Contains(t, ce.c.View().StoodDown, "this node was removed from install "+pkiInstall)
	})
}

// EgressSetOf is the agent's egress set.
func (ce *clientEnv) EgressSetOf() []netip.AddrPort {
	var out []netip.AddrPort
	for _, t := range ce.agent.EgressSet() {
		out = append(out, netip.AddrPortFrom(t.Addr, t.Port))
	}
	return out
}

func TestRefusalsThatAreNotDefinitiveAreWaitedOut(t *testing.T) {
	for _, r := range []agentproto.Refusal{
		{Code: agentproto.RefusalOtherSource, Message: "the certificate of node pve1 came from 10.9.9.9"},
		{Message: "the registry could not be read"},
	} {
		t.Run(r.Code, func(t *testing.T) {
			ce, srv := oneController(t, pki.KindHost)
			ce.apply(ce.desired())
			srv.auth.refuseAlways(&r)
			ce.run(t)
			require.Eventually(t, func() bool { return len(srv.auth.seen()) >= 3 }, waitFor, tick, "it asks again")
			if r.Code != "" {
				require.Eventually(t, func() bool {
					return slices.ContainsFunc(ce.c.View().Problems, func(p string) bool { return strings.Contains(p, "not one of this node's") })
				}, waitFor, tick, "the agent is told what kind of refusal it is, not the controller's detail")
			}
			require.Empty(t, ce.c.View().StoodDown)
			require.Equal(t, uint64(1), ce.agent.Status(t.Context()).SeqApplied, "it serves what it was last told")

			srv.auth.refuseAlways(nil)
			srv.ctrl.publish(ce.desired())
			require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 2 }, waitFor, tick)
		})
	}
}

func TestAnAgentTheControllerSharesNoVersionWithKeepsServing(t *testing.T) {
	ce, srv := oneController(t, pki.KindHost)
	ce.apply(ce.desired())
	ce.c.protocol = agentproto.Range{Min: 2, Max: 3}
	ce.run(t)
	require.Eventually(t, func() bool {
		return slices.ContainsFunc(ce.c.View().Problems, func(p string) bool {
			return strings.Contains(p, "this agent speaks protocol versions 2 to 3 and this controller 1 to 1")
		})
	}, waitFor, tick)
	require.Empty(t, ce.c.View().StoodDown)
	require.Equal(t, uint64(1), ce.agent.Status(t.Context()).SeqApplied)
	require.Empty(t, srv.ctrl.reported())
}

// twoHolders are controllers on pve2 at 127.0.0.1 and on pve3 at ::1, on one
// port, as the registry gives every node the same one.
func twoHolders(t *testing.T, ca pki.CA) (*ctrlServer, *ctrlServer) {
	t.Helper()
	for range 5 {
		a := startController(t, ca, "pve2", netip.MustParseAddrPort("127.0.0.1:0"))
		require.NotNil(t, a)
		b := startController(t, ca, "pve3", netip.AddrPortFrom(netip.IPv6Loopback(), a.addr.Port()))
		if b != nil {
			return a, b
		}
	}
	t.Skip("no port is free on both loopback addresses")
	return nil, nil
}

func TestTheClientFollowsANewHolder(t *testing.T) {
	var a, b *ctrlServer
	ce := newClientEnv(t, pki.KindHost, func(ce *clientEnv) Discoverer {
		a, b = twoHolders(t, ce.ca)
		saveAgentAddress(t, ce.store, "pve2", netip.MustParseAddr("127.0.0.1"))
		saveAgentAddress(t, ce.store, "pve3", netip.IPv6Loopback())
		saveLeader(t, ce.store, "pve2", ce.ca.SHA256())
		return HostDiscovery{Node: testNode, InstallID: pkiInstall, Store: ce.store, Port: a.addr.Port()}
	})
	ce.run(t)
	require.Eventually(t, func() bool { return len(a.ctrl.reported()) > 0 }, waitFor, tick)
	require.Empty(t, b.ctrl.reported())

	saveLeader(t, ce.store, "pve3", ce.ca.SHA256())
	require.Eventually(t, func() bool { return len(b.ctrl.reported()) > 0 }, waitFor, tick)
	require.Equal(t, "pve3", ce.c.View().Controller)
	seen := len(a.ctrl.reported())
	b.ctrl.publish(ce.desired())
	require.Eventually(t, func() bool { return ce.agent.Status(t.Context()).SeqApplied == 1 }, waitFor, tick)
	require.Len(t, a.ctrl.reported(), seen, "the old holder hears nothing more")
}

func TestTheHolderOnThisNodeIsNotDialled(t *testing.T) {
	var srv *ctrlServer
	var counter *countingDiscover
	ce := newClientEnv(t, pki.KindHost, func(ce *clientEnv) Discoverer {
		srv = startController(t, ce.ca, "pve2", netip.MustParseAddrPort("127.0.0.1:0"))
		saveAgentAddress(t, ce.store, "pve2", netip.MustParseAddr("127.0.0.1"))
		saveLeader(t, ce.store, testNode, ce.ca.SHA256())
		counter = &countingDiscover{Discoverer: HostDiscovery{Node: testNode, InstallID: pkiInstall, Store: ce.store, Port: srv.addr.Port()}}
		return counter
	})
	ce.run(t)
	require.Eventually(t, func() bool { return counter.count() >= 5 }, waitFor, tick)
	require.Empty(t, srv.auth.seen(), "nothing was dialled while this node holds the lease")
	require.Equal(t, testNode+" (this node)", ce.c.View().Controller)
	server, err := ce.files.Server()
	require.NoError(t, err, "the holder has its server certificate issued at start")
	require.Equal(t, []string{pki.NodeServerName(pkiInstall, testNode)}, server.Cert.DNSNames)

	saveLeader(t, ce.store, "pve2", ce.ca.SHA256())
	require.Eventually(t, func() bool { return len(srv.ctrl.reported()) > 0 }, waitFor, tick)
}

func TestANewCAIsFollowedWithinAWatch(t *testing.T) {
	var srv *ctrlServer
	ce := newClientEnv(t, pki.KindHost, func(ce *clientEnv) Discoverer {
		srv = startController(t, ce.ca, "pve2", netip.MustParseAddrPort("127.0.0.1:0"))
		saveAgentAddress(t, ce.store, "pve2", netip.MustParseAddr("127.0.0.1"))
		saveLeader(t, ce.store, "pve2", ce.ca.SHA256())
		return HostDiscovery{Node: testNode, InstallID: pkiInstall, Store: ce.store, Port: srv.addr.Port()}
	})
	ce.run(t)
	require.Eventually(t, func() bool { return len(srv.ctrl.reported()) > 0 }, waitFor, tick)

	next := newCA(t, time.Now().Add(-day))
	require.NoError(t, ce.store.SaveCA(caFiles(t, next), new(caFiles(t, ce.ca)), time.Now()))
	srv.trust(next.Cert)
	srv.srv.SetCertificate(serverCert(t, next, "pve2"))
	saveLeader(t, ce.store, "pve2", next.SHA256())

	require.Eventually(t, func() bool {
		return slices.ContainsFunc(srv.auth.seen(), func(c *x509.Certificate) bool { return c.CheckSignatureFrom(next.Cert) == nil })
	}, waitFor, tick, "the agent comes back with a certificate of the new CA")
	creds, err := ce.files.Client()
	require.NoError(t, err)
	require.Equal(t, next.SHA256(), creds.CASHA256())
	en, _, err := ce.store.Enrolment(testNode)
	require.NoError(t, err)
	require.Equal(t, pki.Fingerprint(creds.Cert), en.SHA256)
	require.Contains(t, ce.events.messages()[len(ce.events.messages())-1], "issued this node's certificates again under the CA "+next.SHA256())
}

func TestEventsGoWithTheStatusUntilAcknowledged(t *testing.T) {
	ce, srv := oneController(t, pki.KindHost)
	ce.log.Add(Event{At: time.Now(), Level: levelWarn, Kind: kindNode, Subject: testNode, Message: "something to tell"})
	ce.run(t)
	require.Eventually(t, func() bool { return len(srv.ctrl.reported()) >= 4 }, waitFor, tick)
	carried := 0
	for _, st := range srv.ctrl.reported() {
		for _, raw := range st.Events {
			if strings.Contains(string(raw), "something to tell") {
				carried++
			}
		}
	}
	require.Equal(t, 1, carried, "sent once, then acknowledged")

	t.Run("a status the controller refuses as too large gives its events up", func(t *testing.T) {
		big := make([]byte, 700<<10)
		_, err := rand.Read(big)
		require.NoError(t, err)
		ce.log.Add(Event{At: time.Now(), Level: levelInfo, Kind: kindNode, Message: hex.EncodeToString(big)})
		ce.log.Add(Event{At: time.Now(), Level: levelInfo, Kind: kindNode, Message: "after it"})
		ce.c.Trigger()
		before := len(srv.ctrl.reported())
		require.Eventually(t, func() bool { return len(srv.ctrl.reported()) >= before+3 }, waitFor, tick)
		for _, st := range srv.ctrl.reported()[before:] {
			for _, raw := range st.Events {
				require.Less(t, len(raw), 1<<20)
			}
		}
	})
}

func TestAnApplianceAgentRenewsItsCertificateOverTheChannel(t *testing.T) {
	ce, srv := oneController(t, pki.KindAppliance)
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: testNode, VMID: 200}
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	old, err := ce.ca.IssueClient(csr, id, time.Now().Add(-61*day), rand.Reader)
	require.NoError(t, err)
	require.NoError(t, ce.files.SaveClient(Creds{Key: key, Cert: old, CA: ce.ca.Cert}))
	srv.ctrl.mu.Lock()
	srv.ctrl.renew = func(_ string, csr []byte) ([]byte, error) {
		cert, err := ce.ca.IssueClient(csr, id, time.Now(), rand.Reader)
		if err != nil {
			return nil, err
		}
		return []byte(pemCert(cert)), nil
	}
	srv.ctrl.mu.Unlock()
	ce.run(t)

	require.Eventually(t, func() bool {
		creds, err := ce.files.Client()
		return err == nil && creds.Cert.SerialNumber.Cmp(old.SerialNumber) != 0
	}, waitFor, tick)
	creds, err := ce.files.Client()
	require.NoError(t, err)
	require.False(t, creds.Key.PublicKey.Equal(&key.PublicKey), "a renewal comes with a new key")
	require.Eventually(t, func() bool {
		return slices.ContainsFunc(srv.auth.seen(), func(c *x509.Certificate) bool { return c.Equal(creds.Cert) })
	}, waitFor, tick, "the agent dials with the renewed certificate")
	for _, st := range srv.ctrl.reported() {
		require.Equal(t, 200, st.VMID, "the status names the container its certificate does")
	}
}

func TestARenewedCertificateIsCheckedBeforeItIsKept(t *testing.T) {
	ca := newCA(t, time.Now().Add(-day))
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: testNode, VMID: 200}
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	was := Creds{CA: ca.Cert}
	good := func(t *testing.T, id pki.Identity, under pki.CA) string {
		t.Helper()
		csr, err := pki.NewCSR(key, id, rand.Reader)
		require.NoError(t, err)
		cert, err := under.IssueClient(csr, id, time.Now(), rand.Reader)
		require.NoError(t, err)
		return pemCert(cert)
	}
	_, err = renewed(key, clusterapi.CertificateResponse{CertPEM: good(t, id, ca)}, id, was)
	require.NoError(t, err)

	other := pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: "pve3", VMID: 200}
	_, err = renewed(key, clusterapi.CertificateResponse{CertPEM: good(t, other, ca)}, id, was)
	require.ErrorContains(t, err, "is of")
	_, err = renewed(key, clusterapi.CertificateResponse{CertPEM: good(t, id, newCA(t, time.Now().Add(-day)))}, id, was)
	require.ErrorContains(t, err, "not under the CA")
	otherKey, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	_, err = renewed(otherKey, clusterapi.CertificateResponse{CertPEM: good(t, id, ca)}, id, was)
	require.ErrorContains(t, err, "not of the new key")
	_, err = renewed(key, clusterapi.CertificateResponse{CertPEM: "junk"}, id, was)
	require.Error(t, err)
}

func TestTheClientFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			t.Error("the redirect was followed")
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/agent/desired", nil)
	require.NoError(t, err)
	resp, err := httpClient(srv.Client().Transport).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusFound, resp.StatusCode)
}
