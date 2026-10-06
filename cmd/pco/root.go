package main

import (
	"context"
	"encoding/json"
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
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
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

// askHelp is what the help of a command that asks the daemon says of who may
// run it and of the exit status 2.
const askHelp = "It asks the daemon through its socket, which answers only root and pco-web, the user of the\n" +
	"web interface; the exit status is 2 when the daemon could not be asked."

// commandGroups are the groups of pco --help, in the order they are shown,
// with the commands of each.
var commandGroups = []struct {
	id, title string
	commands  []string
}{
	{"start", "Getting started:", []string{"setup", "credential", "apply", "status"}},
	{"routes", "Routes and guests:", []string{"routes", "plan", "route", "guest", "claims", "adopt", "segment", "diagnose"}},
	{"operate", "Operating:", []string{"sync", "events", "doctor", "settings", "egress", "net", "tunnel", "web", "daemon", "upgrade"}},
	{"lifecycle", "Lifecycle:", []string{"uninstall", "appliance", "version", "completion"}},
}

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
	// The appliance's web interface: webDir holds its certificate, net0File
	// the address of net0, which it listens on.
	webDir, net0File string

	// daemon is what pco daemon is run with: the parts of the daemon that a
	// test replaces.
	daemon daemon.Deps
	// upgrade is what pco upgrade works with.
	upgrade upgradeEnv
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
		webDir:      webcert.Dir,
		net0File:    webcert.Net0File,
		upgrade:     defaultUpgradeEnv(),
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
		Use:   "pco",
		Short: "Cloudflare Tunnel operator for Proxmox VE",
		Long: "Cloudflare Tunnel operator for Proxmox VE. pco publishes the hostnames that tagged guests\n" +
			"list in their Notes through Cloudflare Tunnels: it keeps the DNS records, the tunnels and a\n" +
			"cloudflared connector for each tunnel in line with the Notes. pco daemon does that work.\n" +
			"Most other commands ask it through its socket, which answers root; the others work on the\n" +
			"machine directly, and the help of each says who may run it.\n\n" + exitHelp,
		Example: "  # What pco found and did on this node\n" +
			"  pco status\n\n" +
			"  # The help of a command\n" +
			"  pco help route manual add",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	flags := root.PersistentFlags()
	flags.StringVar(&a.socket, "socket", daemon.DefaultSocket,
		"unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write")
	flags.BoolVar(&a.json, "json", false,
		"print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, "+
			"doctor, credential list, add and check, settings show and apply, route manual list and add): printed as "+
			"the daemon sent it, re-indented, with control and bidirectional characters escaped; "+
			"version prints the build and the schema version of the store")

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
		a.upgradeCmd(),
		a.completionCmd(),
	)
	groupOf := map[string]string{}
	for _, g := range commandGroups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
		for _, name := range g.commands {
			groupOf[name] = g.id
		}
	}
	for _, c := range root.Commands() {
		c.GroupID = groupOf[c.Name()]
	}
	return root
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Long: "Print the build version. With --json it is a JSON object that also names the schema\n" +
			"version of the store this build reads and writes, which pco upgrade --rollback asks the\n" +
			"pco it would go back to. It does not ask the daemon, and anyone may run it.",
		Example: "  # The version, the commit and the date of this build\n" +
			"  pco version\n\n" +
			"  # As JSON, with the schema version of the store\n" +
			"  pco version --json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.json {
				return printVersionJSON(cmd.OutOrStdout())
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("pco %s (%s, %s)\n", version.Version, version.Commit, version.Date)
			return s.done()
		},
	}
}

// versionJSON is what pco version --json prints.
type versionJSON struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Date          string `json:"date"`
	SchemaVersion int    `json:"schemaVersion"` // of the store
}

func printVersionJSON(w io.Writer) error {
	raw, err := json.Marshal(versionJSON{
		Version: version.Version, Commit: version.Commit, Date: version.Date, SchemaVersion: store.SchemaVersion(),
	})
	if err != nil {
		return err
	}
	return printJSON(w, raw)
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
