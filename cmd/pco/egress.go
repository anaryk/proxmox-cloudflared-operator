package main

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// egressEnv is what the egress commands take from the node. They work on its
// ruleset and its local state directly, without the daemon and its socket.
type egressEnv struct {
	nft   egress.Nft
	local string // the local state root, where the overrides are kept
	euid  func() int
	uid   func() (uint32, error) // of the connector user
}

func defaultEgressEnv() egressEnv {
	return egressEnv{
		nft:   egress.NewNft(),
		local: store.DefaultPaths().Local,
		euid:  os.Geteuid,
		uid:   egress.ConnectorUID,
	}
}

// loadedText is what the connectors reach once a table with empty sets is
// loaded.
const loadedText = "the connectors reach Cloudflare's edge and nothing else until the daemon adds their verified targets"

func (a *app) egressCmd() *cobra.Command { return a.egressCmdWith(defaultEgressEnv()) }

func (a *app) egressCmdWith(e egressEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Show and control the filter that confines the connectors",
		Long: "The egress filter is the nftables table inet pco_egress. It lets the processes of the user\n" +
			egress.ConnectorUser + ", which the connectors run as, open connections only to the targets the\n" +
			"daemon verified, to the resolvers of the node on port 53 and to Cloudflare's edge, so that\n" +
			"whoever changes a tunnel at Cloudflare cannot point a connector at anything else the node\n" +
			"reaches. These commands work on this node directly, without the daemon, and need root.",
	}
	cmd.AddCommand(
		a.egressLoadCmd(e), a.egressShowCmd(e),
		a.egressBlockCmd(e), a.egressUnblockCmd(e),
		a.egressOffCmd(e), a.egressOnCmd(e),
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
		Short:  "Load the egress table with empty sets, as the boot unit does",
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
			loaded, err := egress.Load(cmd.Context(), e.nft, uid)
			if err != nil {
				return err
			}
			if loaded {
				s.printf("Loaded the egress table: %s.\n", loadedText)
			} else {
				s.println("The egress table is loaded already; its sets were kept.")
			}
			return s.done()
		},
	}
}

func (a *app) egressShowCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show whether the egress filter is on, what its sets hold and what it rejected",
		Args:  cobra.NoArgs,
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
			live, err := egress.ReadLive(cmd.Context(), e.nft)
			loaded := err == nil
			if err != nil && !errors.Is(err, egress.ErrNotLoaded) {
				return err
			}
			return a.renderEgress(cmd.OutOrStdout(), egressView{
				off: off, since: since, loaded: loaded, live: live, blocked: blocked,
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
}

func (a *app) renderEgress(w io.Writer, v egressView) error {
	s := &screen{w: w}
	switch {
	case v.off && v.loaded:
		s.printf("The egress filter is switched off since %s, but its table is loaded; the daemon does not keep it up to date. pco egress on switches it back on.\n", a.since(v.since))
	case v.off:
		s.printf("The egress filter is switched off since %s: the connectors are not confined. pco egress on switches it back on.\n", a.since(v.since))
	case !v.loaded:
		s.println("The egress filter is on, but its table is not loaded: the connectors are not confined. pco egress on loads it.")
	default:
		s.println("The egress filter is on.")
	}
	if v.loaded {
		s.println("")
		var tg, rs []string
		for _, t := range v.live.Targets {
			tg = append(tg, t.String())
		}
		for _, r := range v.live.Resolvers {
			rs = append(rs, r.String())
		}
		printList(s, "Targets", tg)
		printList(s, "Resolvers", rs)
		s.println("")
		s.println("Rejected since the table was loaded:")
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
	return s.done()
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
			"The list is subtracted from every set the daemon loads, until pco egress unblock.",
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
			removed, err := egress.Drop(cmd.Context(), e.nft, addr)
			switch {
			case errors.Is(err, egress.ErrNotLoaded):
				s.println("The egress table is not loaded.")
			case err != nil:
				return err
			case removed == 0:
				s.println("The egress table held no entry for it.")
			case removed == 1:
				s.println("Took 1 entry for it out of the egress table.")
			default:
				s.printf("Took %d entries for it out of the egress table.\n", removed)
			}
			return s.done()
		},
	}
}

func (a *app) egressUnblockCmd(e egressEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "unblock <address>",
		Short: "Take an address off the block list of this node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			addr, err := parseEgressAddr(args[0])
			if err != nil {
				return err
			}
			removed, err := egress.NewOverrides(e.local).Unblock(addr)
			if err != nil {
				return err
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
			"until pco egress on. The connectors are not confined meanwhile.",
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
		Long: "Let the daemon and the boot unit load the egress table again, and load it with empty sets\n" +
			"unless it is loaded: the daemon adds the verified targets at its next cycle.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.egressCheck(cmd, e); err != nil {
				return err
			}
			// Before anything changes: without the user there is nothing to load.
			uid, err := e.uid()
			if err != nil {
				return err
			}
			wasOff, err := egress.NewOverrides(e.local).SwitchOn()
			if err != nil {
				return err
			}
			loaded, err := egress.Load(cmd.Context(), e.nft, uid)
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
			return s.done()
		},
	}
}
