// Command cffake serves the in-memory Cloudflare of internal/cfapi/cffake over
// HTTP, to try pco against it by hand:
//
//	go run ./hack/cffake -zone example.com:acct1 -token secret
//
// then point pco at http://127.0.0.1:8787/client/v4. Nothing is kept past the
// end of the process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// zones collects the repeated -zone flag.
type zones []string

func (z *zones) String() string     { return strings.Join(*z, ",") }
func (z *zones) Set(v string) error { *z = append(*z, v); return nil }

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
		fmt.Fprintln(os.Stderr, "cffake:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8787", "address to listen on")
	token := flag.String("token", "", "bearer token to accept; any token when empty")
	var specs zones
	flag.Var(&specs, "zone", "a zone and the account that owns it, as name:account; may be repeated")
	flag.Parse()

	fake := cffake.New()
	for i, spec := range specs {
		name, account, ok := strings.Cut(spec, ":")
		if !ok || name == "" || account == "" {
			return fmt.Errorf("-zone %q is not name:account", spec)
		}
		fake.AddAccount(account, account)
		fake.AddZone(fmt.Sprintf("zone-%d", i+1), name, account)
	}

	logger := log.New(os.Stdout, "", log.Ltime)
	api := cffake.Handler(fake, cffake.WithToken(*token))
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			api.ServeHTTP(sw, r)
			// The escaped path cannot carry a line break; the headers are not shown.
			logger.Printf("%s %s %d", r.Method, r.URL.EscapedPath(), sw.status)
		}),
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	logger.Printf("serving http://%s/client/v4", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
