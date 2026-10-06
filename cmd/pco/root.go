package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

// errReported is returned by a command that has printed what is wrong itself:
// main only turns it into the exit code.
var errReported = errors.New("reported")

// couldNotAsk is the error of a command that got no answer of the daemon it
// could read: it exits with 2, like one that could not reach the daemon.
type couldNotAsk struct{ error }

func (e couldNotAsk) Unwrap() error { return e.error }

// exitHelp is what the help says of the exit status, which scripts act on.
const exitHelp = `Exit status:
  0  all is well
  1  the command ran and found something to look at: problems in the state, a failed
     doctor check or diagnosis step, a request the daemon refused; or it failed otherwise
  2  it could not ask the daemon: the daemon is not running, its socket refused the
     connection, no answer came in time, or the answer could not be read`

// env is what the commands take from the machine they run on. The zero parts
// are filled by defaultEnv; tests replace them.
type env struct {
	now     func() time.Time
	loc     *time.Location // the zone times are shown in
	version string         // of this binary

	// stdinTerminal says whether in is a terminal, and which file descriptor
	// to read a secret from when it is.
	stdinTerminal func(in io.Reader) (fd int, ok bool)
	readPassword  func(fd int) ([]byte, error)
	// stderrTerminal says whether w, where the daemon logs, is a terminal.
	stderrTerminal func(w io.Writer) bool
	getenv         func(name string) string
	// profileFile is the marker that names the profile of the machine.
	profileFile string

	// daemon is what pco daemon is run with: the parts of the daemon that a
	// test replaces.
	daemon daemon.Deps
}

func defaultEnv() env {
	return env{
		now:     time.Now,
		loc:     time.Local,
		version: version.Version,
		stdinTerminal: func(in io.Reader) (int, bool) {
			f, ok := in.(*os.File)
			if !ok || !term.IsTerminal(int(f.Fd())) {
				return 0, false
			}
			return int(f.Fd()), true
		},
		readPassword: readSecret,
		stderrTerminal: func(w io.Writer) bool {
			f, ok := w.(*os.File)
			return ok && term.IsTerminal(int(f.Fd()))
		},
		getenv:      os.Getenv,
		profileFile: store.ProfileFile,
	}
}

// app is the state the commands share: the global flags and the machine.
type app struct {
	env
	socket string
	json   bool
}

func newRootCmd() *cobra.Command { return newRootCmdWith(defaultEnv()) }

func newRootCmdWith(e env) *cobra.Command {
	a := &app{env: e}
	root := &cobra.Command{
		Use:           "pco",
		Short:         "Cloudflare Tunnel operator for Proxmox VE",
		Long:          "Cloudflare Tunnel operator for Proxmox VE.\n\n" + exitHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	flags := root.PersistentFlags()
	flags.StringVar(&a.socket, "socket", daemon.DefaultSocket,
		"unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write")
	flags.BoolVar(&a.json, "json", false,
		"print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, "+
			"doctor, credential list, add and check, settings show and apply, route manual list and add): printed as "+
			"the daemon sent it, re-indented, with control and bidirectional characters escaped")

	root.AddCommand(
		a.versionCmd(),
		a.daemonCmd(),
		a.statusCmd(),
		a.routesCmd(),
		a.planCmd(),
		a.eventsCmd(),
		a.applyCmd(),
		a.adoptCmd(),
		a.syncCmd(),
		a.tunnelCmd(),
		a.credentialCmd(),
		a.claimsCmd(),
		a.guestCmd(),
		a.settingsCmd(),
		a.routeCmd(),
		a.segmentCmd(),
		a.diagnoseCmd(),
		a.doctorCmd(),
		a.setupCmd(),
		a.uninstallCmd(),
		a.egressCmd(),
		a.netCmd(),
		a.webCmd(),
		a.applianceCmd(),
	)
	return root
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("pco %s (%s, %s)\n", version.Version, version.Commit, version.Date)
			return s.done()
		},
	}
}

// noJSON is the check of a command that has no answer of the daemon to print:
// --json would be taken and do nothing, which says nothing of what the admin
// meant.
func (a *app) noJSON(cmd *cobra.Command) error {
	if a.json {
		return fmt.Errorf("--json has no meaning for %q: it prints no answer of the daemon", cmd.CommandPath())
	}
	return nil
}

// client returns a client of the daemon on the socket of the flags.
func (a *app) client() *apiclient.Client { return apiclient.New(a.socket) }

// explain turns the error of a call into what the admin is told. A daemon that
// does not know the request is another version than this binary, and the
// versions say so.
func (a *app) explain(ctx context.Context, err error) error {
	if !errors.Is(err, apiclient.ErrUnknownRequest) {
		return err
	}
	theirs := "unknown"
	if v, verr := a.client().Version(ctx); verr == nil {
		theirs = v
	}
	return couldNotAsk{fmt.Errorf("the daemon does not know this command; pco and the daemon are different versions (cli %s, daemon %s)", a.version, theirs)}
}
