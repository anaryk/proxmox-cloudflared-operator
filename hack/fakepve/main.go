// Command fakepve serves the Proxmox VE of internal/web/pvefake over TLS, so
// that pco web can sign users in without a node:
//
//	go run ./hack/fakepve -users users.json -cert-dir /tmp/fakepve -web-cert
//	pco web --pin /tmp/fakepve/pveproxy.crt --pve-url https://127.0.0.1:8006/api2/json \
//	  --cert /tmp/fakepve/tls.crt --key /tmp/fakepve/tls.key ...
//
// users.json lists the users, their privileges by path, the guests they may
// see, the tickets (PVE:<user>:<anything>::<signature>, as the PVEAuthCookie
// of the browser) and the API tokens they sign in with:
//
//	{"users": [{
//	  "user": "alice@pve",
//	  "privileges": {"/": ["Sys.Audit", "Sys.Modify"]},
//	  "guests": ["qemu/101", "lxc/200"],
//	  "tickets": ["PVE:alice@pve:66F1E2D3::YWxpY2U="],
//	  "tokens": [{"id": "pco", "secret": "0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d",
//	              "privsep": true, "privileges": {"/": ["Sys.Audit"]}, "guests": ["lxc/200"]}]
//	}]}
//
// The certificate is made at start and written to pveproxy.crt in the
// directory; its key stays in memory. With -web-cert a second one, for pco
// web to serve, is written there as tls.crt and tls.key, as setup writes
// them on a node. Nothing else is kept past the end of the process.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
)

// statusWriter remembers the status of an answer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakepve:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8006", "address to listen on")
	users := flag.String("users", "", "the users, their privileges, guests, tickets and tokens (JSON)")
	certDir := flag.String("cert-dir", "", "directory to write the certificate to, as pveproxy.crt")
	node := flag.String("node", "pve1", "the node the guests are on")
	webCert := flag.Bool("web-cert", false, "also write a certificate and key for pco web, as tls.crt and tls.key")
	flag.Parse()
	if *users == "" || *certDir == "" {
		return errors.New("-users and -cert-dir are required")
	}

	fake, err := pvefake.Load(*users, *node)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	pair, certPEM, err := pvefake.SelfSigned(time.Now(), host, "localhost", *node)
	if err != nil {
		return err
	}
	certFile := filepath.Join(*certDir, "pveproxy.crt")
	if err := os.MkdirAll(*certDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return err
	}
	if *webCert {
		if err := writeWebCert(*certDir, host, *node); err != nil {
			return err
		}
	}

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			fake.ServeHTTP(sw, r)
			// No header and no query: they carry the tickets and tokens.
			log.Info().Str("method", r.Method).Str("path", r.URL.EscapedPath()).Int("status", sw.status).Msg("request")
		}),
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	log.Info().Str("url", "https://"+ln.Addr().String()+"/api2/json").Str("pin", certFile).Msg("serving")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		grace, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- srv.Shutdown(grace)
	}()
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-stopped
}

// writeWebCert writes a certificate for pco web and its key, made for the
// same names as the fake's own.
func writeWebCert(dir string, names ...string) error {
	pair, certPEM, err := pvefake.SelfSigned(time.Now(), append(names, "localhost")...)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}
