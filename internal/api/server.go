// Package api serves the state of the daemon and its admin actions as JSON over
// a unix socket. Whoever may connect is decided by the peer credentials of the
// socket; the API itself has no login, and it never answers with a secret.
package api

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	readHeaderTimeout = 5 * time.Second
	// A credential check or an apply may take a minute; a read timeout would
	// cancel the request context of such a call, so there is none.
	writeTimeout    = 70 * time.Second
	idleTimeout     = time.Minute
	shutdownTimeout = 5 * time.Second
	bodyReadTimeout = 30 * time.Second
)

// Engine is what the API asks of the engine.
type Engine interface {
	State() engine.State
	Events(since time.Time) []engine.Event
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
	ApproveGuest(ctx context.Context, owner, identity string) (engine.Approval, error)
	RevokeGuest(ctx context.Context, owner string) error
	// Diagnose and Doctor run in the daemon, against the state of the
	// engine and the host it runs on.
	Diagnose(ctx context.Context, hostname string) ([]doctor.Step, error)
	Doctor(ctx context.Context) []doctor.Finding
}

// Server answers API requests for an engine.
type Server struct {
	engine      Engine
	version     string
	allowedUIDs []uint32
	log         zerolog.Logger
	handler     http.Handler

	// checkPeers is whether a request is answered only when its peer is one
	// of allowedUIDs. It starts as what the platform can do; tests set it.
	checkPeers bool
	// bodyTimeout is how long a peer has to send the body of a POST.
	bodyTimeout time.Duration
	// shutdownTimeout is how long Serve waits for running requests.
	shutdownTimeout time.Duration
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
	}
	s.handler = s.routes()
	return s
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

func (s *Server) httpServer() *http.Server {
	srv := &http.Server{
		Handler:           s.handler,
		ConnContext:       connContext,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          stdlog.New(httpErrorLog{s.log}, "", 0),
	}
	if s.onShutdown != nil {
		srv.RegisterOnShutdown(s.onShutdown)
	}
	return srv
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
