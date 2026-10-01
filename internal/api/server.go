// Package api serves the state of the daemon and its admin actions as JSON over
// a unix socket. Whoever may connect is decided by the peer credentials of the
// socket; the API itself has no login, and it never answers with a secret.
package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	readHeaderTimeout = 5 * time.Second
	// A credential check or an apply may take a minute; a read timeout would
	// cancel the request context of such a call, so there is none.
	writeTimeout    = 70 * time.Second
	shutdownTimeout = 5 * time.Second
	probeTimeout    = time.Second

	socketMode = 0o660
	dirMode    = 0o750
)

// Engine is what the API asks of the engine.
type Engine interface {
	State() engine.State
	Events(since time.Time) []engine.Event
	Trigger()
	Apply(ctx context.Context, confirmDeletes bool) error
	Adopt(ctx context.Context, name string) error
	AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error)
	CheckCredential(ctx context.Context, id string, deep bool) (engine.CredentialView, error)
	RemoveCredential(ctx context.Context, id string) error
}

// Server answers API requests for an engine.
type Server struct {
	engine      Engine
	version     string
	allowedUIDs []uint32
	log         zerolog.Logger
	handler     http.Handler

	// shutdownTimeout is how long Serve waits for running requests; tests
	// shorten it.
	shutdownTimeout time.Duration
	// onListening runs once the socket is ready; tests wait on it.
	onListening func()
}

// New returns a server. allowedUIDs are the users whose requests are answered
// where peer credentials can be read; version is what /v1/version reports.
func New(e Engine, version string, allowedUIDs []uint32, log zerolog.Logger) *Server {
	s := &Server{
		engine:          e,
		version:         version,
		allowedUIDs:     slices.Clone(allowedUIDs),
		log:             log,
		shutdownTimeout: shutdownTimeout,
	}
	s.handler = s.routes()
	return s
}

// Handler returns the whole API, the peer check included. A request is
// answered only when the context of its connection says who the peer is, which
// Serve arranges.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.handler,
		ConnContext:       connContext,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
	}
}

// Serve listens on the unix socket path, fixing owner and mode, until ctx
// ends. A clean shutdown returns nil.
func (s *Server) Serve(ctx context.Context, socketPath string, gid int) error {
	ln, err := s.listen(ctx, socketPath, gid)
	if err != nil {
		return err
	}
	srv := s.httpServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	if s.onListening != nil {
		s.onListening()
	}

	select {
	case err := <-done:
		return fmt.Errorf("serving on %s: %w", socketPath, err)
	case <-ctx.Done():
	}
	// The listener's close removes the socket file.
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

// listen makes the socket directory, clears a stale socket and binds the path.
func (s *Server) listen(ctx context.Context, path string, gid int) (net.Listener, error) {
	if err := s.makeDir(filepath.Dir(path), gid); err != nil {
		return nil, err
	}
	if err := clearStale(ctx, path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	s.chown(path, gid)
	return ln, nil
}

// makeDir creates the socket directory when it is missing. A directory that is
// already there is left as it is.
func (s *Server) makeDir(dir string, gid int) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	// MkdirAll is subject to the umask.
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", dir, err)
	}
	// The group of the socket must be able to reach it.
	s.chown(dir, gid)
	return nil
}

// chown hands path to root and gid, as far as the process may: a daemon that
// does not run as root, as in tests, keeps what it has.
func (s *Server) chown(path string, gid int) {
	err := os.Chown(path, 0, gid)
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		s.log.Warn().Err(err).Str("path", path).Int("gid", gid).Msg("could not change the owner")
	}
}

// clearStale removes a socket file nobody listens on. It refuses to touch
// anything that is not a socket, and a socket another process answers on.
func clearStale(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("checking %s: %w", path, err)
	case info.Mode().Type() != fs.ModeSocket:
		return fmt.Errorf("%s exists and is not a socket; not removing it", path)
	}
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("another process is already listening on %s", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case !errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("checking whether %s is in use: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the stale socket %s: %w", path, err)
	}
	return nil
}

type peerKey struct{}

// withPeerUID returns ctx carrying the uid of the process on the other end of
// the connection.
func withPeerUID(ctx context.Context, uid uint32) context.Context {
	return context.WithValue(ctx, peerKey{}, uid)
}

func peerUID(ctx context.Context) (uint32, bool) {
	uid, ok := ctx.Value(peerKey{}).(uint32)
	return uid, ok
}
