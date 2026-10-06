package main

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// netEnv is what the net commands take from the appliance. They work on its
// network directly, without the daemon.
type netEnv struct {
	nl   appnet.Netlink
	nft  egress.Nft
	euid func() int
}

func defaultNetEnv() netEnv {
	return netEnv{nl: appnet.NewNetlink(), nft: appnet.NewNft(), euid: os.Geteuid}
}

func (a *app) netCmd() *cobra.Command { return a.netCmdWith(defaultNetEnv()) }

func (a *app) netCmdWith(e netEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "net",
		Short: "Show how the appliance keeps the service prefix to itself",
		Long: "The service prefix " + appnet.ServicePrefix.String() + " never leaves the appliance: pco-net.service routes it\n" +
			"to the dummy device " + appnet.Device + " before the connectors start, and the nftables table\n" +
			appnet.Table + " rejects whatever is still sent to it, with or without the route. The daemon\n" +
			"loads both again when they are changed. These commands run in the appliance and need root.\n\n" +
			"While pco runs, pco net load puts back what is missing. Never restart pco-net.service for\n" +
			"that: pco and every connector restart with it.\n\n" +
			"After a boot at which pco-net.service failed, pco and the connectors, which require it, did\n" +
			"not start, and pco net load starts neither. Once pco net show names nothing a load cannot\n" +
			"put back, systemctl start pco.service starts pco-net.service again and then pco, which\n" +
			"starts the connectors once it has proved the container is the appliance.",
	}
	cmd.AddCommand(a.netLoadCmd(e), a.netShowCmd(e))
	return cmd
}

// netCheck is the check every net command makes first.
func (a *app) netCheck(cmd *cobra.Command, e netEnv) error {
	if err := a.noJSON(cmd); err != nil {
		return err
	}
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile != store.ProfileAppliance {
		return errors.New("pco net is for the appliance profile")
	}
	if e.euid() != 0 {
		return errors.New("pco net needs root: it reads and changes the network of the appliance")
	}
	return nil
}

func (a *app) netLoadCmd(e netEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "load",
		Short: "Put the service-prefix route and table in place, as the boot unit does",
		Long: "Put in place what pco-net.service keeps: the dummy device " + appnet.Device + " with the address " +
			appnet.ServiceSource.String() + ",\n" +
			"the route of " + appnet.ServicePrefix.String() + " through it with that source, the policy rules at " +
			strconv.Itoa(appnet.PrefUnreachable) + " and " + strconv.Itoa(appnet.PrefToPrefix) + ",\n" +
			"and the table " + appnet.Table + ". What is there already stays, the table with its counter. It stops\n" +
			"at the first step that fails and exits with 1: run by pco-net.service at boot, that fails the\n" +
			"unit, and pco and the connectors do not start.",
		Example: "  # Put back what is missing while pco runs\n" +
			"  pco net load\n" +
			"  # Then see that everything is in place\n" +
			"  pco net show\n" +
			"  # After a boot at which pco-net.service failed: load it, then pco and the connectors\n" +
			"  systemctl start pco.service",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.netCheck(cmd, e); err != nil {
				return err
			}
			changed, err := loadNet(cmd.Context(), e)
			s := &screen{w: cmd.OutOrStdout()}
			switch {
			case len(changed) > 0:
				s.println("Loaded what pco-net.service keeps in place:")
				for _, c := range changed {
					s.printf("  %s\n", c)
				}
			case err == nil:
				s.println("Everything pco-net.service keeps in place is there.")
			}
			if werr := s.done(); werr != nil {
				return werr
			}
			return err
		},
	}
}

// loadNet is what pco-net.service runs at every boot, step by step, before
// the connectors start: a step that fails fails the unit, and they stay down.
func loadNet(ctx context.Context, e netEnv) (changed []string, err error) {
	return appnet.Load(ctx, e.nl, e.nft)
}

func (a *app) netShowCmd(e netEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the device, route, rules and table that keep the service prefix in the appliance",
		Long: "Show each thing pco-net.service keeps in place and whether it is, and how many packets the\n" +
			"table " + appnet.Table + " rejected since it was loaded: each was sent to the service prefix\n" +
			"and would otherwise have looked for it beyond the appliance. It changes nothing, and exits\n" +
			"with 1 when anything is missing or not as pco loads it.",
		Example: "  # In the appliance\n" +
			"  pco net show\n" +
			"  # On the node, for the appliance in container 9250\n" +
			"  pct exec 9250 -- pco net show",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.netCheck(cmd, e); err != nil {
				return err
			}
			parts, err := appnet.Inspect(cmd.Context(), e.nl, e.nft)
			if err != nil {
				return err
			}
			leaked, lerr := appnet.Leaked(cmd.Context(), e.nft)
			if lerr != nil && !errors.Is(lerr, appnet.ErrNotLoaded) {
				return lerr
			}
			return renderNet(&screen{w: cmd.OutOrStdout()}, parts, leaked, lerr == nil)
		},
	}
}

func renderNet(s *screen, parts []appnet.Part, leaked egress.Counter, counted bool) error {
	var differences []string
	for _, p := range parts {
		differences = append(differences, p.Differences...)
	}
	if len(differences) == 0 {
		s.printf("The service prefix %s stays in the appliance: it is routed to %s, and the table %s rejects what is still sent to it.\n",
			appnet.ServicePrefix, appnet.Device, appnet.Table)
	} else {
		s.printf("The service prefix %s is not kept in the appliance as pco-net.service keeps it; pco net load puts it back:\n",
			appnet.ServicePrefix)
		for _, d := range differences {
			s.printf("  %s\n", d)
		}
	}
	s.println("")
	t := s.table()
	for _, p := range parts {
		t.row("  "+p.Name, partState(p))
	}
	t.flush()
	s.println("")
	if counted {
		s.printf("Rejected since the table was loaded: %s\n", packets(leaked.Packets))
	} else {
		s.println("Rejected since the table was loaded: unknown, the table is not loaded")
	}
	if err := s.done(); err != nil {
		return err
	}
	if len(differences) > 0 {
		return errReported
	}
	return nil
}

// partState is what show says of a part: in place, missing, or there but not
// as pco-net.service leaves it.
func partState(p appnet.Part) string {
	switch {
	case p.OK():
		return "in place"
	case p.Missing:
		return "missing"
	}
	return "not as pco loads it"
}
