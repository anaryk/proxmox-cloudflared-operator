// Package web is the process behind pco web: it serves the interface over
// HTTPS, and the mounters add the session and the gateway to the daemon.
//
// Nothing in this process lists the host's network interfaces or their
// addresses: Go asks the kernel for them through an AF_NETLINK socket, which
// the unit's RestrictAddressFamilies leaves out, so under the sandbox such a
// call fails. The node's names and addresses come from the flags and from the
// environment that setup writes.
package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	stdlog "log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/daemon"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

// DefaultListen is the address without PCO_WEB_LISTEN.
const DefaultListen = "127.0.0.1:8643"

// The server has no ReadTimeout or WriteTimeout: they would cut the streams.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 5 * time.Second
	maxHeaderBytes    = 64 << 10
)

// Mounter adds its routes to the router. The session and the gateway are
// mounters, and the server knows neither type.
type Mounter interface{ Mount(r gin.IRouter) }

// Config is what the web process serves, and where.
type Config struct {
	Listen   string
	Hosts    []string // accepted Host names besides the node's names and the listen address
	CertFile string   // pco web's default: $CREDENTIALS_DIRECTORY/tls.crt
	KeyFile  string   // pco web's default: $CREDENTIALS_DIRECTORY/tls.key
	HSTS     bool     // PCO_WEB_HSTS=1
	Assets   fs.FS    // dist as built: ui.Assets(), or an fstest.MapFS in tests
	Now      func() time.Time
	Log      zerolog.Logger
}

// Server is the web process.
type Server struct {
	cfg     Config
	handler http.Handler
	// listening is called once the listener is up; tests set it.
	listening func(net.Addr)
	// shutdownAfter is how long a stop waits for the requests; tests shorten it.
	shutdownAfter time.Duration
}

// New returns a server for cfg with the routes of mounts. The build is read
// and compressed here, once.
func New(cfg Config, mounts ...Mounter) (*Server, error) {
	if cfg.Assets == nil {
		return nil, errors.New("no interface to serve")
	}
	files, err := loadAssets(cfg.Assets)
	if err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, shutdownAfter: shutdownTimeout}
	s.handler = s.routes(files, mounts)
	return s, nil
}

// Handler returns everything the server answers.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes(files *assets, mounts []Mounter) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	// The client is the TCP peer: nothing stands in front of pco web, and a
	// header such as X-Forwarded-For is whatever the client wrote.
	r.ForwardedByClientIP = false
	r.RemoteIPHeaders = nil
	r.Use(s.logRequests, s.recoverPanics, securityHeaders(s.cfg.HSTS))
	r.NoRoute(notFound)
	r.NoMethod(func(c *gin.Context) { plain(c, http.StatusMethodNotAllowed, "method not allowed") })
	for _, m := range mounts {
		m.Mount(r)
	}
	files.mount(r)
	return r
}

// logRequests logs every answer with its method, path, status, duration and
// the TCP peer: at Info when it was a failure, 4xx or 5xx, otherwise at Debug.
// Never the query, a header or the body: they carry tickets, tokens and the
// session cookie.
func (s *Server) logRequests(c *gin.Context) {
	start := s.cfg.Now()
	c.Next()
	status := c.Writer.Status()
	event := s.cfg.Log.Debug()
	if status >= http.StatusBadRequest {
		event = s.cfg.Log.Info()
	}
	event.
		Str("method", c.Request.Method).
		Str("path", c.Request.URL.Path).
		Int("status", status).
		Dur("duration", s.cfg.Now().Sub(start)).
		Str("peer", c.Request.RemoteAddr).
		Msg("request")
}

// recoverPanics answers a panicking handler with a 500. It logs the stack but
// not the value, which may carry what the handler held: a ticket or a token.
func (s *Server) recoverPanics(c *gin.Context) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v)
		}
		s.cfg.Log.Error().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Bytes("stack", debug.Stack()).
			Msg("request handler panicked")
		if c.Writer.Written() {
			c.Abort()
			return
		}
		c.Writer.Header().Del("Content-Encoding")
		plain(c, http.StatusInternalServerError, "internal error")
	}()
	c.Next()
}

func notFound(c *gin.Context) { plain(c, http.StatusNotFound, "not found") }

func plain(c *gin.Context, status int, msg string) {
	c.Header("Cache-Control", cacheNever)
	c.Data(status, "text/plain; charset=utf-8", []byte(msg+"\n"))
	c.Abort()
}

// Run serves on the listen address until ctx ends, and tells systemd it is
// ready once it listens. The certificate is read once: the daemon restarts
// the unit when it changes.
//
// On a stop the requests are ended first, so that the streams, which would
// never finish by themselves, let the connections go. What still holds on after
// the grace period is cut off, and that is a stop all the same: it returns nil.
func (s *Server) Run(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("reading the certificate %s and its key %s: %w", s.cfg.CertFile, s.cfg.KeyFile, err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}
	requests, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	srv := &http.Server{
		Handler: s.handler,
		// The context of every request descends from this one.
		BaseContext: func(net.Listener) context.Context { return requests },
		// HTTP/2 is on: ServeTLS offers it unless TLSNextProto is set.
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// "OPTIONS *" goes to the router, which answers it with the headers
		// of every answer.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     stdlog.New(httpErrorLog{s.cfg.Log}, "", 0),
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()

	s.cfg.Log.Info().Str("listen", ln.Addr().String()).Msg("serving the web interface")
	if s.listening != nil {
		s.listening(ln.Addr())
	}
	s.notify(sd.SdNotifyReady)

	select {
	case err := <-done:
		return fmt.Errorf("serving on %s: %w", s.cfg.Listen, err)
	case <-ctx.Done():
	}
	s.notify(sd.SdNotifyStopping)
	endRequests()
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownAfter)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		s.cfg.Log.Warn().Err(err).Msg("the shutdown did not finish, closing the connections")
		_ = srv.Close()
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving on %s: %w", s.cfg.Listen, err)
	}
	return nil
}

// notify speaks sd_notify; outside a unit of Type=notify there is no socket
// and it does nothing.
func (s *Server) notify(state string) {
	if _, err := sd.SdNotify(false, state); err != nil {
		s.cfg.Log.Warn().Err(err).Str("state", state).Msg("telling systemd")
	}
}

// httpErrorLog carries what net/http would print to the standard logger,
// failed TLS handshakes among it, into the log.
type httpErrorLog struct{ log zerolog.Logger }

func (w httpErrorLog) Write(p []byte) (int, error) {
	w.log.Warn().Str("component", "http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
