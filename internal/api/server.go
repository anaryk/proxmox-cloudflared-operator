// Package api serves the state of the daemon and its admin actions as JSON over
// a unix socket. Whoever may connect is decided by the peer credentials of the
// socket; the API itself has no login, and it never answers with a secret.
package api

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	readHeaderTimeout = 5 * time.Second
	// A credential check or an apply may take a minute; a read timeout would
	// cancel the request context of such a call, so there is none.
	writeTimeout    = 70 * time.Second
	idleTimeout     = time.Minute
	shutdownTimeout = 5 * time.Second
	bodyReadTimeout = 30 * time.Second

	// bootHeader names the process of the daemon in every answer.
	bootHeader = "Pco-Boot"
)

// Engine is what the API asks of the engine.
type Engine interface {
	State() engine.State
	// Boot names the process of the daemon, and RunsAs the profile of its
	// install and its node.
	Boot() string
	RunsAs() (profile, node string)
	QueryEvents(q engine.EventQuery) ([]engine.Event, error)
	Subscribe(ctx context.Context, boot string, after uint64) (<-chan engine.Notice, engine.Hello, error)
	Trigger()
	Apply(ctx context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error)
	Adopt(ctx context.Context, name string) error
	RotateTunnel(ctx context.Context, account string) (engine.TunnelRotation, error)
	AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error)
	Credentials() ([]engine.CredentialView, error)
	CheckCredential(ctx context.Context, id string, deep bool) (engine.CredentialView, error)
	RemoveCredential(ctx context.Context, id string) error
	Claims() ([]engine.ClaimView, error)
	ResolveClaim(ctx context.Context, hostname, owner string) error
	Approvals() ([]engine.ApprovalView, error)
	ApproveGuest(ctx context.Context, owner, identity string, macs []string, addrs []netip.Addr) (engine.Approval, error)
	RevokeGuest(ctx context.Context, owner string) error
	Segments() ([]engine.SegmentView, error)
	AcknowledgeSegment(ctx context.Context, bridge string, vlan int) error
	RevokeSegment(ctx context.Context, bridge string, vlan int) error
	// Diagnose and Doctor run in the daemon, against the state of the
	// engine and the host it runs on.
	Diagnose(ctx context.Context, hostname string) ([]doctor.Step, error)
	Doctor(ctx context.Context) []doctor.Finding
	// PollInterval is the interval of the last settings read.
	PollInterval() time.Duration
	// Traffic is what the last scrapes of the connectors on the node gave,
	// and RouteSeries what the counters of the egress filter gave of the
	// target of one route.
	Traffic() engine.TrafficView
	RouteSeries(hostname string) (engine.RouteSeries, error)
	// The settings and the manual routes are written at the revision they
	// were read at.
	SettingsView() (engine.SettingsView, error)
	SaveSettings(ctx context.Context, rev int, s store.Settings) (engine.SettingsView, []string, error)
	ManualRoutes() ([]engine.ManualRouteView, error)
	CreateManualRoute(ctx context.Context, v engine.ManualRouteView) (engine.ManualRouteView, error)
	UpdateManualRoute(ctx context.Context, id string, rev int, v engine.ManualRouteView) (engine.ManualRouteView, error)
	DeleteManualRoute(ctx context.Context, id string, rev int) error
	Guests() ([]engine.GuestListView, error)
	Annotation(ref model.GuestRef) (engine.AnnotationView, error)
	RequestRestart(ctx context.Context) error
	// The networks reached through a leg: those attached, one of them with
	// its service addresses, the bridges and VLANs that may be, the probe
	// of an address on one, and the attach and the detach of a network.
	Networks() ([]engine.NetworkView, error)
	Network(id string) (engine.NetworkDetail, error)
	CandidateNetworks(ctx context.Context) ([]engine.CandidateNetwork, error)
	ProbeNetwork(ctx context.Context, r engine.ProbeRequest) (engine.ProbeResult, error)
	Attach(ctx context.Context, r engine.AttachRequest) (engine.NetworkView, error)
	Detach(ctx context.Context, id string, rev int64, served []string) error
	// The managed network: its view, the consent of a guest and its
	// revocation, and the switch that stores the network once its vnet
	// exists.
	Managed() (engine.ManagedView, error)
	ApproveManaged(ctx context.Context, owner, identity string, acceptFirewallImpact bool) (store.Consent, error)
	RevokeManaged(ctx context.Context, owner string) error
	EnableManaged(ctx context.Context) error
	// Promote shows what differs between the store and the agents, and
	// confirmed draws a new epoch that makes this node the writer.
	Promote(ctx context.Context, req engine.PromoteRequest) (engine.PromoteResult, error)
	// The cluster: its status and its nodes as the last cycle found them,
	// what the admin does to the lease, the upgrade, a block and the whole
	// install, and what the admin does to one node.
	ClusterStatus() engine.ClusterStatus
	Nodes() []engine.NodeView
	Handoff(ctx context.Context, to string) (engine.HandoffResult, error)
	NewJoinToken(ctx context.Context, node string) (engine.JoinTokenView, error)
	FinishUpgrade(ctx context.Context) (engine.FinishUpgradeResult, error)
	Unblock(ctx context.Context, addr netip.Addr) error
	UninstallCluster(ctx context.Context, force bool) (engine.UninstallResult, error)
	AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (engine.NodeView, error)
	DrainNode(ctx context.Context, name string, force bool) (engine.NodeView, error)
	UndrainNode(ctx context.Context, name string) (engine.NodeView, error)
	ForgetNode(ctx context.Context, name string, compromised bool) (engine.ForgetResult, error)
	SetNode(ctx context.Context, name string, p engine.NodePatch) (engine.NodeView, error)
	// The agent of this node: what pco agent status shows, and the reset of
	// the epoch it remembers.
	AgentView() (agentproto.AgentView, error)
	ResetAgentEpoch(ctx context.Context) error
}

// Server answers API requests for an engine.
type Server struct {
	engine      Engine
	version     string
	allowedUIDs []uint32
	webUID      uint32 // the user of the web UI; 0 for none
	log         zerolog.Logger
	handler     http.Handler
	// router is the routes behind the peer check; tests list them.
	router *gin.Engine
	// role is what the daemon says about its node; nil is the controller.
	roleMu sync.RWMutex
	role   Role

	// checkPeers is whether a request is answered only when its peer is one
	// of allowedUIDs. It starts as what the platform can do; tests set it.
	checkPeers bool
	// bodyTimeout is how long a peer has to send the body of a POST.
	bodyTimeout time.Duration
	// shutdownTimeout is how long Serve waits for running requests.
	shutdownTimeout time.Duration
	// writeTimeout is the server's, for a whole answer; a stream moves it
	// on with every write.
	writeTimeout time.Duration
	// ticker starts the pings of a stream; tests replace it.
	ticker func(every time.Duration) (<-chan time.Time, func())
	// onListening runs once the socket is ready, and onShutdown when a
	// shutdown begins; tests wait on them.
	onListening func()
	onShutdown  func()
}

// New returns a server. allowedUIDs are the users whose requests are answered
// where peer credentials can be read; version is what /v1/version reports.
func New(e Engine, version string, allowedUIDs []uint32, log zerolog.Logger) *Server {
	s := &Server{
		engine:          e,
		version:         version,
		allowedUIDs:     slices.Clone(allowedUIDs),
		log:             log,
		checkPeers:      peerChecks,
		bodyTimeout:     bodyReadTimeout,
		shutdownTimeout: shutdownTimeout,
		writeTimeout:    writeTimeout,
		ticker:          startTicker,
	}
	s.handler = s.routes()
	return s
}

func startTicker(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// OnListening sets the function Serve calls, once, when the socket is ready
// for connections. It has to be set before Serve is called.
func (s *Server) OnListening(fn func()) { s.onListening = fn }

// SetShutdownTimeout sets how long Serve waits for the requests that are
// running when its context ends; what is still running then is cut off, and
// Serve returns an error for which errors.Is(err, context.DeadlineExceeded)
// holds. It has to be set before Serve is called.
func (s *Server) SetShutdownTimeout(d time.Duration) { s.shutdownTimeout = d }

// Handler returns the whole API, the peer check included. A request is
// answered only when the context of its connection says who the peer is, which
// Serve arranges.
func (s *Server) Handler() http.Handler { return s.handler }

// httpServer is the server of one Serve. A shutdown waits for the requests
// that run, but a stream would run on: the requests are told when it begins,
// and the streams end.
func (s *Server) httpServer() *http.Server {
	closing := make(chan struct{})
	srv := &http.Server{
		Handler: s.handler,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(connContext(ctx, c), closingKey{}, (<-chan struct{})(closing))
		},
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      s.writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          stdlog.New(httpErrorLog{s.log}, "", 0),
	}
	srv.RegisterOnShutdown(sync.OnceFunc(func() { close(closing) }))
	if s.onShutdown != nil {
		srv.RegisterOnShutdown(s.onShutdown)
	}
	return srv
}

type closingKey struct{}

// closingOf is closed when the server that took the request of ctx shuts
// down; nil when it is not known.
func closingOf(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(closingKey{}).(<-chan struct{})
	return ch
}

// httpErrorLog carries what net/http would print to the standard logger into
// the daemon's log.
type httpErrorLog struct{ log zerolog.Logger }

func (w httpErrorLog) Write(p []byte) (int, error) {
	w.log.Warn().Str("component", "http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}

// Serve listens on the unix socket path, fixing owner and mode, until ctx
// ends. A clean shutdown returns nil.
//
// The path must be absolute, and the directory of the socket must be named
// "pco": Serve sets the mode and the owner of that directory, which no other
// may have done to it. It makes the directory when it is missing, but not its
// parent, which must exist, belong to the user the daemon runs as and be
// writable by neither group nor others (a symlink to such a directory will do).
// A directory that is already there must be a real one, not a symlink, and owned
// by the user the daemon runs as. Where that is not so, Serve changes nothing
// and fails. Once the directory is open, what is done to it is done through the
// handle, but for binding the socket, which takes a path (see bindPrivate).
//
// Serve takes a lock on the path of the socket, "<path>.lock", for as long as it
// runs, so that two calls on the same path cannot run at once. The lock guards
// this socket and nothing else; a lock that keeps a second daemon from starting
// with another socket path is the caller's to take.
func (s *Server) Serve(ctx context.Context, socketPath string, gid int) error {
	// The mode and the owner of its directory are set: that must never be the
	// directory the daemon happens to run in.
	if !filepath.IsAbs(socketPath) {
		return fmt.Errorf("the socket path %q must be absolute", socketPath)
	}
	sock, err := s.openSocket(ctx, socketPath, gid)
	if err != nil {
		return err
	}
	defer sock.release()

	if !s.checkPeers {
		s.log.Warn().Msg("peer credentials are not checked on this platform; every local user may use the API")
	}
	srv := s.httpServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sock.ln) }()
	if s.onListening != nil {
		s.onListening()
	}

	select {
	case err := <-done:
		return fmt.Errorf("serving on %s: %w", socketPath, err)
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutting down: %w", err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving on %s: %w", socketPath, err)
	}
	return nil
}
