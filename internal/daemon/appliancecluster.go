package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// applianceCluster is what a controller container runs besides the engine:
// the install's CA and its server certificate, and the agent endpoint on
// net0's address, which the agent containers of the other nodes join and
// follow. A connection is taken only from the current addresses of a
// registered agent's container on the node it was enrolled for, or, to join,
// of a container in pool pco on a node with an unused join token.
type applianceCluster struct {
	st      *store.Store
	paths   store.Paths
	install string
	vmid    int
	node    string
	joins   *applianceJoins
	remote  *RemoteFleet
	addrs   func(ctx context.Context) ([]netip.Addr, error)
	port    uint16
	every   time.Duration
	now     func() time.Time
	rand    io.Reader
	log     zerolog.Logger
	eng     *engine.Engine

	mu        sync.Mutex
	listening *clusterRun
	server    *clusterapi.Server
	allow     *clusterapi.Allowlist
	at        netip.AddrPort
}

func newApplianceCluster(cfg Config, deps Deps, app ApplianceDeps, st *store.Store, state applianceState, px joinProxmox) *applianceCluster {
	c := &applianceCluster{
		st: st, paths: cfg.Paths, install: state.install.ID, vmid: state.install.Appliance.VMID, node: state.install.Appliance.Node,
		addrs: app.Net0Addrs, port: cmp.Or(deps.Cluster.Port, uint16(clusterapi.Port)), every: cmp.Or(deps.Cluster.Every, 10*time.Second),
		now: deps.Now, rand: app.Rand, log: cfg.Log,
	}
	c.joins = &applianceJoins{st: st, px: px, install: c.install, node: c.node, now: deps.Now, rand: app.Rand, log: cfg.Log}
	c.remote = newRemoteFleet(deps.Now, func() agentproto.Epoch {
		w, found, err := st.Writer()
		if err != nil || !found {
			return agentproto.Epoch{InstallID: c.install}
		}
		return agentproto.Epoch{InstallID: w.InstallID, Generation: w.Generation, Nonce: w.Nonce}
	})
	return c
}

// wire gives the engine the agents of the other nodes and what it asks of
// the endpoint.
func (c *applianceCluster) wire(ed *engine.Deps) {
	ed.Remote = c.remote
	ed.Close = c.closeNode
	ed.Endpoints = c.endpoints
	ed.Reissue = c.reissue
}

// attach gives the cluster part the engine it serves for.
func (c *applianceCluster) attach(eng *engine.Engine) {
	c.eng = eng
	c.joins.note = eng.NoteEnrol
	c.remote.attach(eng, func(ctx context.Context) agentproto.Status {
		if a := eng.Agent(); a != nil {
			return a.Status(ctx)
		}
		return agentproto.Status{}
	})
}

// run keeps the CA, the certificate and the endpoint until ctx ends.
func (c *applianceCluster) run(ctx context.Context) {
	defer c.stop()
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *applianceCluster) tick(ctx context.Context) {
	creds, err := c.ensureCerts()
	if err != nil {
		c.log.Warn().Err(err).Msg("the agent endpoint has no certificate yet")
		return
	}
	addrs, err := c.addrs(ctx)
	if err != nil || len(addrs) == 0 {
		c.log.Warn().Err(err).Msg("net0 has no address for the agent endpoint yet")
		return
	}
	want := netip.AddrPortFrom(addrs[0], c.port)
	c.mu.Lock()
	running, at, srv, allow := c.listening, c.at, c.server, c.allow
	c.mu.Unlock()
	switch {
	case running != nil && at.Addr() != want.Addr():
		c.stop()
		c.listen(ctx, want, creds)
	case running == nil:
		c.listen(ctx, want, creds)
	default:
		srv.SetCertificate(creds.ServerTLS())
		if err := allow.Refresh(ctx); err != nil {
			c.log.Debug().Err(err).Msg("reading who may reach the agent endpoint failed")
		}
	}
}

func (c *applianceCluster) listen(ctx context.Context, at netip.AddrPort, creds agent.Creds) {
	allow := clusterapi.NewAllowlist(c.sources, c.now, c.log)
	if err := allow.Refresh(ctx); err != nil {
		c.log.Warn().Err(err).Msg("reading who may reach the agent endpoint failed")
	}
	chk := &clusterapi.Checker{
		InstallID: c.install, Store: c.st, Allow: allow, Now: c.now, Log: c.log,
		Record: func(e store.SecurityEvent) {
			e.Node = c.node
			if err := c.st.AppendSecurityEvent(c.node, e); err != nil {
				c.log.Warn().Err(err).Msg("recording a security event failed")
			}
			if c.eng != nil {
				c.eng.NoteEnrol(e.Subject, e.Message)
			}
		},
	}
	srv := clusterapi.New(applianceController{c.remote, c.joins}, chk, creds.ServerTLS(), chk.Pool, c.log)
	srv.AcceptJoins(clusterapi.Joins{
		InstallID: c.install, Tokens: c.st, Misused: c.joins.misused,
		CASHA256: func() string {
			ca, err := c.joins.ca()
			if err != nil {
				return ""
			}
			return ca.SHA256()
		},
	})
	if c.eng != nil {
		srv.OnEvent(c.eng.NoteNode)
	}
	listening := make(chan netip.AddrPort, 1)
	srv.OnListening(func(ap netip.AddrPort) { listening <- ap })
	r := start(ctx, func(ctx context.Context) {
		if err := srv.Serve(ctx, at); err != nil {
			c.log.Error().Err(err).Msg("the agent endpoint stopped")
		}
		close(listening)
	})
	ap, ok := <-listening
	if !ok {
		r.end()
		return
	}
	c.mu.Lock()
	c.listening, c.server, c.allow, c.at = r, srv, allow, ap
	c.mu.Unlock()
	c.log.Info().Stringer("address", ap).Msg("serving the agent containers of the other nodes")
}

func (c *applianceCluster) stop() {
	c.mu.Lock()
	r := c.listening
	c.listening, c.server, c.allow = nil, nil, nil
	c.mu.Unlock()
	if r != nil {
		r.end()
		c.remote.Serve(nil)
	}
}

// applianceController is the controller the endpoint asks: the remote fleet,
// with the joins and renewals of appliance agents.
type applianceController struct {
	*RemoteFleet
	joins *applianceJoins
}

func (a applianceController) Join(ctx context.Context, tok pki.JoinToken, csr []byte, src netip.Addr, facts clusterapi.JoinFacts) ([]byte, error) {
	return a.joins.Join(ctx, tok, csr, src, facts)
}

func (a applianceController) Renew(ctx context.Context, node string, csr []byte) ([]byte, error) {
	return a.joins.Renew(ctx, node, csr)
}

// ensureCerts makes the install's CA when it has none, and the controller's
// server certificate under the current CA when it has none, it is due, or it
// is under another CA.
func (c *applianceCluster) ensureCerts() (agent.Creds, error) {
	if _, _, _, err := c.st.CA(); errors.Is(err, store.ErrNoCA) {
		ca, err := pki.NewCA(c.install, c.now(), c.rand)
		if err != nil {
			return agent.Creds{}, fmt.Errorf("making the install's CA: %w", err)
		}
		certPEM, keyPEM, err := ca.PEM()
		if err != nil {
			return agent.Creds{}, err
		}
		if err := c.st.SaveCAIfAbsent(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}); err != nil &&
			!errors.Is(err, store.ErrRevision) {
			return agent.Creds{}, fmt.Errorf("storing the install's CA: %w", err)
		}
		c.log.Info().Str("sha256", ca.SHA256()).Msg("made the install's CA")
	} else if err != nil {
		return agent.Creds{}, fmt.Errorf("reading the install's CA: %w", err)
	}
	ca, err := c.joins.ca()
	if err != nil {
		return agent.Creds{}, err
	}
	files := agent.TLSFiles{Dir: agent.TLSDir(c.paths)}
	name := pki.ControllerServerName(c.install)
	now := c.now()
	s, err := files.Server()
	if err == nil && len(s.Cert.DNSNames) == 1 && s.Cert.DNSNames[0] == name && s.CASHA256() == ca.SHA256() &&
		!pki.DueForRenewal(s.Cert, now) && !pki.DueForSelfRenewal(s.Cert, now) {
		return s, nil
	}
	key, err := pki.NewKey(c.rand)
	if err != nil {
		return agent.Creds{}, err
	}
	cert, err := ca.IssueServer(&key.PublicKey, []string{name}, now, c.rand)
	if err != nil {
		return agent.Creds{}, fmt.Errorf("issuing the controller's server certificate: %w", err)
	}
	next := agent.Creds{Key: key, Cert: cert, CA: ca.Cert}
	if err := files.SaveServer(next); err != nil {
		return agent.Creds{}, err
	}
	return next, nil
}

// reissue serves a certificate under the install's current CA at once, as a
// rotation of the CA needs.
func (c *applianceCluster) reissue(context.Context) error {
	creds, err := c.ensureCerts()
	if err != nil {
		return err
	}
	c.mu.Lock()
	srv := c.server
	c.mu.Unlock()
	if srv != nil {
		srv.SetCertificate(creds.ServerTLS())
	}
	return nil
}

func (c *applianceCluster) closeNode(node string) {
	c.mu.Lock()
	srv := c.server
	c.mu.Unlock()
	if srv != nil {
		srv.Close(node)
	}
}

// endpoints is where the agents find this controller: the address it
// listens on; none before it listens.
func (c *applianceCluster) endpoints() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listening == nil {
		return nil
	}
	return []string{c.at.String()}
}

// sources are the addresses the endpoint takes a connection from: of every
// registered appliance agent that is not removed, the addresses of its
// container while it runs on the node it was enrolled for and is no HA
// resource; and to join, those of the containers in pool pco on a node an
// unused join token is for, but this one.
func (c *applianceCluster) sources(ctx context.Context) (clusterapi.Sources, error) {
	nodes, err := c.st.Nodes()
	if err != nil {
		return clusterapi.Sources{}, err
	}
	res, err := c.joins.px.Resources(ctx)
	if err != nil {
		return clusterapi.Sources{}, err
	}
	ha, err := c.joins.px.HAResources(ctx)
	if err != nil {
		return clusterapi.Sources{}, err
	}
	src := clusterapi.Sources{Nodes: map[string][]netip.Addr{}}
	for _, n := range nodes {
		if n.Kind != string(pki.KindAppliance) || n.VMID == 0 || n.State == store.NodeRemoved {
			continue
		}
		if b := appliance.BoundTo(n.VMID, n.Name, res, ha); !b.Bound {
			continue
		}
		if addrs, err := c.containerAddrs(ctx, n.Name, n.VMID); err == nil {
			src.Nodes[n.Name] = addrs
		}
	}
	tokens, err := c.st.JoinTokens()
	if err != nil {
		return src, nil
	}
	now := c.now()
	open := map[string]bool{}
	for _, t := range tokens {
		if len(t.Uses) == 0 && now.Before(t.Expires) {
			open[t.Node] = true
		}
	}
	for _, r := range res {
		if r.Kind != model.KindLXC || r.Pool != appliance.Pool || r.VMID == c.vmid || !open[r.Node] {
			continue
		}
		if addrs, err := c.containerAddrs(ctx, r.Node, r.VMID); err == nil {
			src.Join = append(src.Join, addrs...)
		}
	}
	slices.SortFunc(src.Join, func(a, b netip.Addr) int { return a.Compare(b) })
	src.Join = slices.Compact(src.Join)
	return src, nil
}

func (c *applianceCluster) containerAddrs(ctx context.Context, node string, vmid int) ([]netip.Addr, error) {
	cfg, err := c.joins.px.GuestConfig(ctx, node, model.GuestRef{Kind: model.KindLXC, VMID: vmid})
	if err != nil {
		return nil, err
	}
	return c.joins.addrsOf(ctx, node, vmid, cfg)
}
