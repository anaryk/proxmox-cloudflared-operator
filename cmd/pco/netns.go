package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// netnsEnv is what the netns commands take from the node. They work on its
// network directly, without the daemon.
type netnsEnv struct {
	backend func(log zerolog.Logger) gateway.Backend
	local   string // the local state root, where the cache of the gateway is kept
	euid    func() int
	now     func() time.Time
	// connectors says how many connectors run, daemon whether pco.service
	// does.
	connectors func(ctx context.Context) (int, error)
	daemon     func(ctx context.Context) (bool, error)
}

func defaultNetnsEnv() netnsEnv {
	return netnsEnv{
		backend: func(log zerolog.Logger) gateway.Backend {
			return gateway.NewHost(gateway.NewNamespaceNft(gateway.Namespace), gateway.NewHostNft(), log)
		},
		local:      store.DefaultPaths().Local,
		euid:       os.Geteuid,
		now:        time.Now,
		connectors: runningConnectors,
		daemon:     daemonRuns,
	}
}

func (a *app) netnsCmd() *cobra.Command { return a.netnsCmdWith(defaultNetnsEnv()) }

func (a *app) netnsCmdWith(e netnsEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "netns",
		Short: "Show the gateway namespace the attached networks are reached through",
		Long: "pco reaches a network the node has no address in through a leg of its own in that network.\n" +
			"The legs live in the network namespace " + gateway.Namespace + ", which a veth pair links to the host; the\n" +
			"connectors dial a service address of " + gateway.ServicePrefix.String() + " for each target behind a leg, and the\n" +
			"namespace NATs it to the target through its leg. pco-netns.service brings the namespace back at\n" +
			"boot from what the daemon last applied, before the guests start; the daemon keeps it after\n" +
			"that. The unit has no stop action: a restart only brings up again what is missing and leaves\n" +
			"the legs and the open connections as they are. These commands run on the node and need root.",
		Example: "  # Whether the namespace, its legs and its tables are as pco left them\n" +
			"  pco netns status\n\n" +
			"  # After a change made by hand: bring back what pco-netns.service brings up\n" +
			"  systemctl restart pco-netns",
	}
	cmd.AddCommand(a.netnsUpCmd(e), a.netnsStatusCmd(e), a.netnsSyncCmd(e), a.netnsDownCmd(e))
	return cmd
}

// netnsCheck is the check every netns command makes first.
func (a *app) netnsCheck(cmd *cobra.Command, e netnsEnv) error {
	if err := a.noJSON(cmd); err != nil {
		return err
	}
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile != store.ProfileHost {
		return errors.New("pco netns is for the host profile: in the appliance the legs are network cards of the container")
	}
	if e.euid() != 0 {
		return errors.New("pco netns needs root: it reads and changes the network namespaces of the node")
	}
	return nil
}

func (a *app) netnsUpCmd(e netnsEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Bring the gateway namespace up and restore its legs, as the boot unit does",
		Long: "Bring up the namespace " + gateway.Namespace + " with its sysctls, the transit link, the routes of the service\n" +
			"prefix on the host, the table of the namespace and the host's table, then the legs, the\n" +
			"service addresses and the sticky entries the node's cache holds, and the pinned neighbours\n" +
			"while they are fresh. It reads everything before it changes it and changes only what\n" +
			"differs, so that a second run changes nothing and open connections stay open. It prints a\n" +
			"line for each leg and one for the service addresses, and exits with 1 when the namespace\n" +
			"cannot be brought up. pco-netns.service runs it at boot and at every restart.",
		Example: "  # What pco-netns.service runs\n" +
			"  pco netns up\n\n" +
			"  # The same through the unit, which has no stop action\n" +
			"  systemctl restart pco-netns",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.netnsCheck(cmd, e); err != nil {
				return err
			}
			b := e.backend(a.daemonLog(cmd.ErrOrStderr(), zerolog.InfoLevel))
			c, _, cerr := gateway.ReadCache(e.local)
			if cerr != nil {
				// Without the cache the namespace, its tables and the fallback
				// route come up all the same, so that no request to a service
				// address leaves the node by its default route; no Sync, which
				// would take away the legs that are up.
				if err := b.Up(cmd.Context()); err != nil {
					return fmt.Errorf("bringing up the gateway namespace: %w; and %w", err, cerr)
				}
				return fmt.Errorf("the gateway namespace is up without legs or service addresses until the daemon restores them: %w", cerr)
			}
			states, err := gateway.Restore(cmd.Context(), b, c, e.now())
			if err != nil {
				return fmt.Errorf("bringing up the gateway namespace: %w", err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			printLegs(s, c.Legs, states)
			printServices(s, c.Services, c.Fresh(e.now()), len(c.Pins))
			return s.done()
		},
	}
}

// printLegs prints a line for each leg: up with its address and bridge, or
// why not.
func printLegs(s *screen, legs []gateway.Leg, states []gateway.LegState) {
	if len(legs) == 0 {
		s.println("No network is attached: the namespace has its transit link and its tables only.")
		return
	}
	for i, l := range legs {
		where := l.Bridge
		if l.VLAN != 0 {
			where += " VLAN " + strconv.Itoa(l.VLAN)
		}
		var st gateway.LegState
		if i < len(states) {
			st = states[i]
		}
		switch {
		case st.Up && st.Problem == "":
			s.printf("leg %s up %s on %s\n", l.Iface(), l.Prefix, where)
		case st.Up:
			s.printf("leg %s up %s on %s: %s\n", l.Iface(), l.Prefix, where, st.Problem)
		default:
			s.printf("leg %s down on %s: %s\n", l.Iface(), where, st.Problem)
		}
	}
}

func printServices(s *screen, services []gateway.Service, freshPins bool, pins int) {
	ports := 0
	for _, svc := range services {
		ports += len(svc.Ports)
	}
	s.printf("service addresses: %d, with %d published ports\n", len(services), ports)
	switch {
	case pins > 0 && freshPins:
		s.printf("pinned neighbours: %d\n", pins)
	case pins > 0:
		s.println("pinned neighbours: those in the cache are older than five minutes; the daemon pins them anew")
	}
}

func (a *app) netnsStatusCmd(e netnsEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the legs of the gateway namespace and whether it is as pco left it",
		Long: "Show the legs, the service addresses and the sticky entries of the node's cache of the\n" +
			"gateway, which the daemon writes after every change, and compare the namespace " + gateway.Namespace + ", its\n" +
			"links, addresses, routes and rules, the transit link, the routes of the service prefix on\n" +
			"the host, both tables with their elements and the sticky entries with it. It changes nothing,\n" +
			"and exits with 1 when anything differs; systemctl restart pco-netns brings back what is\n" +
			"missing.",
		Example: "  # On the node\n" +
			"  pco netns status\n\n" +
			"  # Bring back what it names as missing\n" +
			"  systemctl restart pco-netns",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.netnsCheck(cmd, e); err != nil {
				return err
			}
			c, _, err := gateway.ReadCache(e.local)
			if err != nil {
				return err
			}
			b := e.backend(zerolog.Nop())
			gateway.Expect(b, c)
			verr := b.Verify(cmd.Context())
			var changed *gateway.ChangedError
			if verr != nil && !errors.As(verr, &changed) {
				return verr
			}
			return renderNetns(&screen{w: cmd.OutOrStdout()}, c, changed)
		},
	}
}

func renderNetns(s *screen, c gateway.Cache, changed *gateway.ChangedError) error {
	if len(c.Legs) == 0 {
		s.println("Legs: none")
	} else {
		s.println("Legs:")
		t := s.table()
		for _, l := range c.Legs {
			vlan := "-"
			if l.VLAN != 0 {
				vlan = strconv.Itoa(l.VLAN)
			}
			t.row("  "+l.Iface(), l.ID, l.Bridge, "VLAN "+vlan, l.Prefix.String(), l.MAC.String())
		}
		t.flush()
	}
	s.printf("Service addresses: %d\n", len(c.Services))
	if len(c.Sticky) > 0 {
		s.println("Sticky entries:")
		t := s.table()
		for _, st := range c.Sticky {
			port := st.Port
			if port == "" {
				port = "(guest stopped)"
			}
			t.row("  "+st.MAC.String(), st.Bridge, port)
		}
		t.flush()
	}
	s.println("")
	if changed == nil {
		s.println("The namespace " + gateway.Namespace + " is as pco left it.")
		return s.done()
	}
	s.println("The namespace " + gateway.Namespace + " is not as pco left it; systemctl restart pco-netns brings back what is missing:")
	for _, d := range changed.Differences {
		s.printf("  %s\n", d)
	}
	if err := s.done(); err != nil {
		return err
	}
	return errReported
}

func (a *app) netnsSyncCmd(e netnsEnv) *cobra.Command {
	var from string
	var yes bool
	cmd := &cobra.Command{
		Use:   "sync --from <file>",
		Short: "Make the legs, service addresses, pins and sticky entries those of a file",
		Long: "Bring the namespace up, then make its legs, service addresses, pinned neighbours and\n" +
			"sticky entries those of a JSON file, as the daemon does with what it verified, and keep\n" +
			"them in the node's cache, which pco netns up restores at boot. It is for testing the\n" +
			"gateway without the daemon; the daemon replaces all of it at its next cycle. The file has\n" +
			"the form of the cache: legs (id, mark, bridge, vlan, prefix, mac, managed, uplink),\n" +
			"services (leg, service, addr, ports), pins (leg, addr, mac) and sticky (mac, bridge, vlan,\n" +
			"port). While pco.service runs it refuses unless --yes: the daemon keeps the gateway as the\n" +
			"store says, and the two would undo each other's changes.",
		Example: "  # Two legs and their services from a file\n" +
			"  pco netns sync --from legs.json\n\n" +
			"  # Then see that the namespace holds them\n" +
			"  pco netns status",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.netnsCheck(cmd, e); err != nil {
				return err
			}
			if !yes {
				runs, err := e.daemon(cmd.Context())
				if err != nil {
					return err
				}
				if runs {
					return errors.New("pco.service runs and keeps the gateway as the store says: stop it first, or pass --yes")
				}
			}
			data, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			var c gateway.Cache
			if err := json.Unmarshal(data, &c); err != nil {
				return fmt.Errorf("reading %s: %w", from, err)
			}
			b := e.backend(a.daemonLog(cmd.ErrOrStderr(), zerolog.InfoLevel))
			if err := b.Up(cmd.Context()); err != nil {
				return fmt.Errorf("bringing up the gateway namespace: %w", err)
			}
			states, err := b.Sync(cmd.Context(), c.Legs, c.Services, c.Pins, c.Sticky)
			if err != nil {
				return err
			}
			c.PinsAt = e.now()
			if err := gateway.WriteCache(e.local, c); err != nil {
				return fmt.Errorf("keeping the cache: %w", err)
			}
			s := &screen{w: cmd.OutOrStdout()}
			printLegs(s, c.Legs, states)
			printServices(s, c.Services, true, len(c.Pins))
			return s.done()
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "the JSON file with the legs, services, pins and sticky entries")
	cmd.Flags().BoolVar(&yes, "yes", false, "sync while pco.service runs")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

func (a *app) netnsDownCmd(e netnsEnv) *cobra.Command {
	var uninstall, yes bool
	cmd := &cobra.Command{
		Use:   "down --uninstall",
		Short: "Remove the gateway namespace, its legs and its routes",
		Long: "Remove the legs, the transit link, both routes of the service prefix, the host's table, the\n" +
			"sticky entries of managed guests and the namespace " + gateway.Namespace + " with its table. pco uninstall runs\n" +
			"it; without --uninstall it refuses, so that it is never a stop action of a unit: with the\n" +
			"routes gone, a connector's request to a service address would follow the default route.\n" +
			"While connectors run and the cache holds service addresses it refuses unless --yes.",
		Example: "  # What pco uninstall runs once the connectors are stopped\n" +
			"  pco netns down --uninstall\n\n" +
			"  # The same while connectors still run\n" +
			"  pco netns down --uninstall --yes",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !uninstall {
				return errors.New("pco netns down removes the gateway; run pco uninstall")
			}
			if err := a.netnsCheck(cmd, e); err != nil {
				return err
			}
			// A cache that cannot be read says nothing of the service
			// addresses: the connectors are asked all the same.
			c, _, cerr := gateway.ReadCache(e.local)
			if (cerr != nil || len(c.Services) > 0) && !yes {
				running, err := e.connectors(cmd.Context())
				if err != nil {
					return err
				}
				if running > 0 {
					what := "the service addresses"
					if cerr == nil {
						what = fmt.Sprintf("the %d service addresses", len(c.Services))
					}
					return fmt.Errorf("%d connectors run and may serve %s of the gateway: "+
						"stop them first, as pco uninstall does, or pass --yes", running, what)
				}
			}
			if err := e.backend(a.daemonLog(cmd.ErrOrStderr(), zerolog.InfoLevel)).Down(cmd.Context()); err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Removed the namespace %s with its legs and table, the transit link, the routes of %s and table inet pco_host.\n",
				gateway.Namespace, gateway.ServicePrefix)
			return s.done()
		},
	}
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "remove the gateway, as pco uninstall does")
	cmd.Flags().BoolVar(&yes, "yes", false, "remove it while connectors run")
	return cmd
}

// daemonRuns reports whether pco.service runs.
func daemonRuns(ctx context.Context) (bool, error) {
	err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "pco.service").Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, fmt.Errorf("asking whether pco.service runs: %w", err)
}

// runningConnectors counts the connector units that run.
func runningConnectors(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--plain", "--no-legend", "--state=active",
		"pco-cloudflared@*").Output()
	if err != nil {
		return 0, fmt.Errorf("listing the connectors: %w", err)
	}
	n := 0
	for line := range strings.Lines(string(out)) {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}
