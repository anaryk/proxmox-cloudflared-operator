package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// egressEnv is what the egress commands take from the node. They work on its
// ruleset and its local state directly, without the daemon and its socket.
type egressEnv struct {
	nft       egress.Nft
	local     string // the local state root, where the overrides are kept
	euid      func() int
	uid       func() (uint32, error) // of the connector user
	resolvers func() ([]netip.Addr, error)
	// dial connects for pco egress probe; nil is a net.Dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func defaultEgressEnv() egressEnv {
	return egressEnv{
		nft:       egress.NewNft(),
		local:     store.DefaultPaths().Local,
		euid:      os.Geteuid,
		uid:       egress.ConnectorUID,
		resolvers: egress.SystemResolvers,
	}
}

// loadedText is what the connectors reach once a table without targets is
// loaded.
const loadedText = "the connectors reach the resolvers of the node and Cloudflare's edge, and nothing else until the daemon adds their verified targets"

func (a *app) egressCmd() *cobra.Command { return a.egressCmdWith(defaultEgressEnv()) }

func (a *app) egressCmdWith(e egressEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Show and control the filter that confines the connectors",
		Long: "The egress filter is the nftables table inet pco_egress. It lets the processes of the user\n" +
			egress.ConnectorUser + ", which the connectors run as, open connections only to the targets the\n" +
			"daemon verified, to the resolvers of the node on port 53 and to Cloudflare's edge, so that\n" +
			"whoever changes a tunnel at Cloudflare cannot point a connector at anything else the node\n" +
			"reaches. These commands work on this node directly, without the daemon, and need root.\n\n" +
			"pco egress load loads the table again when it is gone or not as it should be, and keeps the\n" +
			"sets of an intact one. Never restart pco-egress.service for that: every connector restarts\n" +
			"with it.",
		Example: "  # Whether the filter is on, and what it holds\n" +
			"  pco egress show\n\n" +
			"  # Keep the connectors from 10.0.0.12, whatever the daemon verified\n" +
			"  pco egress block 10.0.0.12",
	}
	cmd.AddCommand(
		a.egressLoadCmd(e), a.egressShowCmd(e),
		a.egressBlockCmd(e), a.egressUnblockCmd(e),
		a.egressOffCmd(e), a.egressOnCmd(e),
		a.egressProbeCmd(e),
	)
	return cmd
}

// egressCheck is the check every egress command makes first.
func (a *app) egressCheck(cmd *cobra.Command, e egressEnv) error {
	if err := a.noJSON(cmd); err != nil {
		return err
	}
	if e.euid() != 0 {
		return errors.New("pco egress needs root: it reads and changes the nftables ruleset and the local state of this node")
	}
	return nil
}

func (a *app) egressLoadCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:    "load",
		Short:  "Load the egress table with the resolvers and no targets, as the boot unit does",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			since, off, err := ov.Off()
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			if off {
				s.printf("The egress filter is switched off since %s; its table was not loaded. pco egress on switches it back on.\n", a.since(since))
				return s.done()
			}
			uid, err := e.uid()
			if err != nil {
				return err
			}
			loaded, found, resolverErr, err := loadEgress(cmd, e, ov, uid)
			if err != nil {
				return err
			}
			if loaded {
				s.printf("Loaded the egress table: %s.\n", loadedText)
			} else {
				s.println("The egress table is loaded already; its sets were kept.")
			}
			if resolverErr != nil {
				// What pco-egress.service runs: its failure keeps the
				// connectors from starting at all.
				s.printf("The resolvers could not be read (%v). pco egress load fails for that: run by pco-egress.service, "+
					"it fails the unit, and the connectors, which require the unit, do not start. "+
					"Once the resolvers can be read, start pco-egress.service again.\n", resolverErr)
				if err := s.done(); err != nil {
					return err
				}
				return errReported
			}
			noResolver(s, loaded, found)
			return s.done()
		},
	}
}

// noResolver says that the table was loaded without a resolver, as the node
// names none: the connectors cannot resolve names then.
func noResolver(s *screen, loaded bool, found int) {
	if loaded && found == 0 {
		s.println("No name server was found in /etc/resolv.conf: the connectors resolve no names until one is there " +
			"and the daemon's next cycle lets them reach it.")
	}
}

// loadEgress loads the table with the resolvers of the node and the blocked
// addresses, unless an intact one is there, and returns how many resolvers
// it found. Resolvers that cannot be read are none, and the error comes back
// beside the result: a table without them is better than none, which the
// connectors cannot start without. A block list that cannot be read loads
// nothing.
func loadEgress(cmd *cobra.Command, e egressEnv, ov *egress.Overrides, uid uint32) (loaded bool, found int, resolverErr, err error) {
	blocked, err := ov.Blocked()
	if err != nil {
		return false, 0, nil, err
	}
	resolvers, resolverErr := e.resolvers()
	if resolverErr != nil {
		resolvers = nil
	}
	loaded, err = egress.Load(cmd.Context(), e.nft, uid, resolvers, blocked)
	return loaded, len(resolvers), resolverErr, err
}

// resolversFailed says that the resolvers could not be read, which is an exit
// status of 1, or that none was found, or returns what the screen returns.
func (a *app) resolversFailed(s *screen, loaded bool, found int, err error) error {
	if err == nil {
		noResolver(s, loaded, found)
		return s.done()
	}
	if loaded {
		s.printf("The resolvers could not be read (%v): the table lets the connectors reach none, so they cannot resolve names until the daemon's next cycle.\n", err)
	} else {
		s.printf("The resolvers could not be read (%v).\n", err)
	}
	if err := s.done(); err != nil {
		return err
	}
	return errReported
}

func (a *app) egressShowCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show whether the egress filter is on, what its sets hold and what it rejected",
		Long: "Show whether the egress filter is on and its table loaded as pco loads it, the targets and\n" +
			"the resolvers its sets hold, how many packets it rejected since the table was last loaded\n" +
			"in full, to the addresses of this node and to anything else, and the block list of this\n" +
			"node. It changes nothing. The exit status is 1 when the filter is off, its table is not\n" +
			"loaded or not as pco loads it, or the connector user does not exist. It runs as root.",
		Example: "  # Whether the filter is on, and what it holds\n" +
			"  pco egress show\n\n" +
			"  # From a script: is the filter on and as pco loads it?\n" +
			"  pco egress show > /dev/null && echo confined",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			since, off, err := ov.Off()
			if err != nil {
				return err
			}
			blocked, err := ov.Blocked()
			if err != nil {
				return err
			}
			// Without the user, what the table holds is still worth
			// seeing; the rule for the user cannot be compared.
			uid, err := e.uid()
			noUser := errors.Is(err, egress.ErrNoConnectorUser)
			if err != nil && !noUser {
				return err
			}
			live, err := egress.ReadLive(cmd.Context(), e.nft, uid)
			loaded := !errors.Is(err, egress.ErrNotLoaded)
			switch {
			case errors.Is(err, egress.ErrUnreadable):
				live.Differences = []string{err.Error()}
			case loaded && err != nil:
				return err
			}
			return a.renderEgress(cmd.OutOrStdout(), egressView{
				off: off, since: since, loaded: loaded, live: live, blocked: blocked, noUser: noUser,
			})
		},
	}
}

type egressView struct {
	off     bool
	since   time.Time
	loaded  bool
	live    egress.Live
	blocked []netip.Addr
	noUser  bool // the connector user does not exist
}

func (a *app) renderEgress(w io.Writer, v egressView) error {
	s := &screen{w: w}
	if v.noUser {
		s.printf("The connector user %s does not exist: install the package or run systemd-sysusers. "+
			"Until it exists, no connector starts, and the rule for the user is not compared.\n\n", egress.ConnectorUser)
	}
	switch {
	case v.off && v.loaded:
		s.printf("The egress filter is switched off since %s, but its table is loaded; the daemon does not keep it up to date. pco egress on switches it back on.\n", a.since(v.since))
	case v.off:
		s.printf("The egress filter is switched off since %s: the connectors are not confined. pco egress on switches it back on.\n", a.since(v.since))
	case !v.loaded:
		s.println("The egress filter is on, but its table is not loaded: the connectors are not confined. pco egress load loads it.")
	case len(v.live.Differences) > 0:
		s.println("The egress filter is on, but its table is not as pco loads it, and the connectors may not be confined. pco egress load loads it again:")
		for _, d := range v.live.Differences {
			s.printf("  %s\n", d)
		}
	default:
		s.println("The egress filter is on.")
	}
	if v.loaded {
		s.println("")
		var tg, rs []string
		for _, t := range v.live.Targets {
			if t.AllowNode {
				tg = append(tg, t.String()+" (allowNode)")
				continue
			}
			tg = append(tg, t.String())
		}
		for _, r := range v.live.Resolvers {
			rs = append(rs, r.String())
		}
		printList(s, "Targets", tg)
		printList(s, "Resolvers", rs)
		s.println("")
		s.println("Rejected since the table was last loaded in full:")
		t := s.table()
		t.row("  to addresses of this node", packets(v.live.RejectedLocal.Packets))
		t.row("  to anything else", packets(v.live.Rejected.Packets))
		t.flush()
	}
	s.println("")
	var bl []string
	for _, b := range v.blocked {
		bl = append(bl, b.String())
	}
	printList(s, "Blocked on this node", bl)
	if err := s.done(); err != nil {
		return err
	}
	// A filter that is off, gone or not as pco loads it is a finding, and so
	// is a connector user that does not exist.
	if v.off || !v.loaded || len(v.live.Differences) > 0 || v.noUser {
		return errReported
	}
	return nil
}

func printList(s *screen, title string, items []string) {
	if len(items) == 0 {
		s.printf("%s: none\n", title)
		return
	}
	s.printf("%s:\n", title)
	for _, it := range items {
		s.printf("  %s\n", it)
	}
}

func packets(n uint64) string {
	if n == 1 {
		return "1 packet"
	}
	return fmt.Sprintf("%d packets", n)
}

// since says when the filter was switched off; a time that could not be read
// is not known.
func (a *app) since(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return a.when(t)
}

// parseEgressAddr reads the address an admin typed. Only the parsed address
// goes on, and it is printed back by Go, never as it was typed.
func parseEgressAddr(arg string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(arg)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an IP address", arg)
	}
	return addr.WithZone("").Unmap(), nil
}

func (a *app) egressBlockCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "block <address>",
		Short: "Keep the connectors from an address, whatever the daemon verified",
		Long: "Add an address to the block list of this node and take it out of the egress table at once.\n" +
			"The list is subtracted from every set the daemon loads, until pco egress unblock. It is\n" +
			"kept in /var/lib/pco/egress-blocked.json, for this node only. It runs as root.",
		Example: "  # Keep the connectors from 10.0.0.12\n" +
			"  pco egress block 10.0.0.12\n\n" +
			"  # And see that the table rejects it\n" +
			"  pco egress show",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			addr, err := parseEgressAddr(args[0])
			if err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			added, err := ov.Block(addr)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			if added {
				s.printf("Blocked %s.\n", addr)
			} else {
				s.printf("%s was blocked already.\n", addr)
			}
			_, off, err := ov.Off()
			if err != nil {
				return err
			}
			if off {
				s.println("The egress filter is off; the block applies once it is switched on.")
				return s.done()
			}
			removed, err := egress.BlockLive(cmd.Context(), e.nft, addr)
			switch {
			case errors.Is(err, egress.ErrNotLoaded):
				s.println("The egress table is not loaded.")
			case err != nil:
				return err
			case removed == 0:
				s.println("The egress table held no entry for it, and now rejects it on every port.")
			case removed == 1:
				s.println("Took 1 entry for it out of the egress table, which now rejects it on every port.")
			default:
				s.printf("Took %d entries for it out of the egress table, which now rejects it on every port.\n", removed)
			}
			return s.done()
		},
	}
}

func (a *app) egressUnblockCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "unblock <address>",
		Short: "Take an address off the block list of this node",
		Long: "Take an address off the block list of this node, and out of the blocked set of the egress\n" +
			"table. The daemon puts it back into the table at its next cycle if it is still a verified\n" +
			"target. It runs as root.",
		Example: "  # Let the connectors reach 10.0.0.12 again, if the daemon verifies it\n" +
			"  pco egress unblock 10.0.0.12\n\n" +
			"  # The block list that is left\n" +
			"  pco egress show",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			addr, err := parseEgressAddr(args[0])
			if err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			removed, err := ov.Unblock(addr)
			if err != nil {
				return err
			}
			_, off, err := ov.Off()
			if err != nil {
				return err
			}
			if !off {
				// Out of the blocked set even when the list did not hold it:
				// the list is what counts.
				if _, err := egress.UnblockLive(cmd.Context(), e.nft, addr); err != nil && !errors.Is(err, egress.ErrNotLoaded) {
					return err
				}
			}
			s := &screen{w: cmd.OutOrStdout()}
			if removed {
				s.printf("Unblocked %s: the daemon puts it back into the egress table at its next cycle if it is still a verified target.\n", addr)
			} else {
				s.printf("%s was not blocked.\n", addr)
			}
			return s.done()
		},
	}
}

func (a *app) egressOffCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Switch the egress filter off, when it is itself the fault",
		Long: "Remove the egress table and keep the daemon and the boot unit from loading it again,\n" +
			"until pco egress on. The connectors are not confined meanwhile; pco status warns of it on\n" +
			"every run. The switch is kept in /var/lib/pco/egress-off.json, for this node only. It runs\n" +
			"as root.",
		Example: "  # Switch the filter off while you look for the fault\n" +
			"  pco egress off\n\n" +
			"  # And on again once it is found\n" +
			"  pco egress on",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			since, off, err := ov.Off()
			if err != nil {
				return err
			}
			// The switch first, so that nothing loads the table again between.
			if !off {
				if err := ov.SwitchOff(a.now()); err != nil {
					return err
				}
			}
			if err := egress.Unload(cmd.Context(), e.nft); err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			if off {
				s.printf("The egress filter was off already, since %s; its table is removed.\n", a.since(since))
			} else {
				s.println("Switched the egress filter off: the connectors are not confined until pco egress on.")
			}
			return s.done()
		},
	}
}

func (a *app) egressOnCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Switch the egress filter back on",
		Long: "Let the daemon and the boot unit load the egress table again, and load it, with the\n" +
			"resolvers of the node and no targets, unless it is loaded as it should be: the daemon adds\n" +
			"the verified targets at its next cycle. The exit status is 1 when the resolvers of the node\n" +
			"could not be read. It runs as root.",
		Example: "  # Switch the filter back on\n" +
			"  pco egress on\n\n" +
			"  # And see what it holds after the next cycle\n" +
			"  pco egress show",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			// Before anything changes: without the user, or with a block
			// list that cannot be read, there is nothing to load, and the
			// switch stays off.
			uid, err := e.uid()
			if err != nil {
				return err
			}
			ov := egress.NewOverrides(e.local)
			if _, err := ov.Blocked(); err != nil {
				return err
			}
			wasOff, err := ov.SwitchOn()
			if err != nil {
				return err
			}
			loaded, found, resolverErr, err := loadEgress(cmd, e, ov, uid)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			switch {
			case wasOff && loaded:
				s.printf("Switched the egress filter on and loaded its table: %s at its next cycle.\n", loadedText)
			case wasOff:
				s.println("Switched the egress filter on; its table is loaded.")
			case loaded:
				s.printf("The egress filter was on already; loaded its table: %s at its next cycle.\n", loadedText)
			default:
				s.println("The egress filter was on already, and its table is loaded.")
			}
			return a.resolversFailed(s, loaded, found, resolverErr)
		},
	}
}

// probeDial is how long pco egress probe waits for a connection.
const probeDial = 5 * time.Second

// egressProbeCmd is what the doctor of the appliance runs as the user of the
// connectors, to see what the egress filter does to that user.
func (a *app) egressProbeCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "probe <address:port>",
		Short: "Connect over TCP as the user running it, and say how it went",
		Long: "Connect to an address and a port over TCP, as the user that runs it, and print one line:\n" +
			doctor.ProbeConnected + ", " + doctor.ProbeRefused + ", or " + strings.TrimSpace(doctor.ProbeFailed) + " and the reason. The exit status is 0 for a connection\n" +
			"and 1 otherwise. pco doctor runs it as " + egress.ConnectorUser + ", which the egress filter confines, in the\n" +
			"appliance: a connection as root would say nothing of the filter. It needs no root.",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := checkAddrPort(args[0]); err != nil {
				return err
			}
			dial := e.dial
			if dial == nil {
				var d net.Dialer
				dial = d.DialContext
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), probeDial)
			defer cancel()
			conn, err := dial(ctx, "tcp", args[0])
			s := &screen{w: cmd.OutOrStdout()}
			switch {
			case err == nil:
				_ = conn.Close()
				s.println(doctor.ProbeConnected)
				return s.done()
			case errors.Is(err, syscall.ECONNREFUSED):
				s.println(doctor.ProbeRefused)
			default:
				s.println(doctor.ProbeFailed + strings.Join(strings.Fields(err.Error()), " "))
			}
			if err := s.done(); err != nil {
				return err
			}
			return errReported
		},
	}
}

// checkAddrPort refuses what is not a host and a port from 1 to 65535.
func checkAddrPort(arg string) error {
	host, port, err := net.SplitHostPort(arg)
	if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q: want an address and a port, as 10.0.0.1:8006", arg)
	}
	return nil
}
