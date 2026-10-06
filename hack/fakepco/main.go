// Command fakepco serves the API of the daemon from a scenario of
// internal/api/apifake, on a unix socket as the daemon does, so that pco web
// and the web interface can be worked on and tested without a node:
//
//	go run ./hack/fakepco -socket /tmp/fake/pco/pco.sock -control 127.0.0.1:7071 -scenario populated
//	curl -X POST 127.0.0.1:7071/streams/pause
//
// The scenario is one of the package's, which -list names, or a directory of
// one. Its times move with the clock, which -now starts at another time than
// the real one, for screenshots and tests that show times. A cycle runs a
// poll interval of the settings after the last one ended and the traffic is
// sampled every 5 s, as at the daemon. The controls, which
// change what is served without any check, listen on loopback only; the
// documentation of apifake.Engine.Control lists them. Nothing is kept past the
// end of the process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/api/apifake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakepco:", err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", "", "the socket to serve the API on, <dir>/pco/pco.sock as the daemon's")
	control := flag.String("control", "127.0.0.1:7071", "the loopback address to serve the controls on")
	scenario := flag.String("scenario", "populated", "a scenario of the package, or a directory of one")
	ver := flag.String("version", version.Version, "the version the daemon reports")
	list := flag.Bool("list", false, "list the scenarios of the package and exit")
	nowFlag := flag.String("now", "", "the time to start the clock at, in RFC 3339; the real time without it")
	flag.Parse()
	if *list {
		for _, name := range apifake.Scenarios() {
			about, err := apifake.About(name)
			if err != nil {
				return err
			}
			fmt.Printf("%-13s %s\n", name, about)
		}
		return nil
	}
	if *socket == "" {
		return errors.New("-socket is required")
	}
	if err := loopback(*control); err != nil {
		return err
	}

	clock, err := clockFrom(*nowFlag)
	if err != nil {
		return err
	}
	engine, err := load(*scenario, clock)
	if err != nil {
		return err
	}
	defer func() { _ = engine.Close() }()

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()
	uid := uint32(os.Getuid())
	srv := api.New(engine, *ver, []uint32{uid}, log)
	if uid != 0 {
		// pco web runs as the same user here, and names the actor where the
		// API reads the peer, on Linux; elsewhere calls are of an unknown
		// actor. As root every actor is root (cli).
		srv.SetWebUID(uid)
	}
	ln, err := net.Listen("tcp", *control)
	if err != nil {
		return err
	}
	controls := &http.Server{Handler: engine.Control(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)
	go func() {
		<-ctx.Done()
		grace, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = controls.Shutdown(grace)
	}()
	go func() {
		if err := controls.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("serving the controls")
			stop()
		}
	}()
	srv.OnListening(func() {
		log.Info().Str("socket", *socket).Str("control", "http://"+ln.Addr().String()).Str("scenario", *scenario).
			Str("boot", engine.Boot()).Msg("serving")
	})
	return srv.Serve(ctx, *socket, os.Getgid())
}

// loopback refuses an address to serve the controls on that is not on
// loopback: whoever reaches them changes what the daemon says.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-control: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("-control %s: the controls listen on a loopback address only, such as 127.0.0.1:7071", addr)
	}
	return nil
}

// load loads a scenario of the package by its name, or that of a directory,
// which is named with a path: ./mine.
func load(scenario string, clock func() time.Time) (*apifake.Engine, error) {
	if strings.ContainsRune(scenario, os.PathSeparator) {
		return apifake.Load(scenario, clock)
	}
	return apifake.Scenario(scenario, clock)
}

// clockFrom is a clock that starts at the time of -now and runs on from
// there; nil, the real one, without it.
func clockFrom(at string) (func() time.Time, error) {
	if at == "" {
		return nil, nil
	}
	start, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return nil, fmt.Errorf("-now %q: want a time in RFC 3339, such as 2026-10-06T09:30:00Z", at)
	}
	began := time.Now()
	return func() time.Time { return start.Add(time.Since(began)) }, nil
}
