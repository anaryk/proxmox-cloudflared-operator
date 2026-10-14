package main

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/applianceinstall"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const convertLong = "Make a combined appliance the controller of a cluster, in place, so that the agent\n" +
	"containers of the other nodes can join it. Its store, its credentials and its routes stay, and\n" +
	"its connectors run on throughout: it goes on serving the node it runs on.\n\n" +
	"Run on the node, as root, with --vmid: it checks that the container is a combined appliance\n" +
	"of pco appliance install and that no other controller serves the cluster, runs pco appliance\n" +
	"convert inside it, which records the kind and restarts the daemon (it then makes the install's\n" +
	"CA and serves the agent endpoint, tcp/8644 on net0's address), tags the container a\n" +
	"controller, opens that port in its firewall to ipset pco-agents alone, and lets its token\n" +
	"read its own storage. --ha makes it a Proxmox HA resource as pco appliance install --ha does;\n" +
	"read that command's help first. A convert that failed is run again to finish.\n\n" +
	"Then make a join token for each other node on the controller, and install an agent there\n" +
	"with it."

const convertExample = `  # On the node of the combined appliance in container 120
  pco appliance convert --vmid 120

  # The same, and make it an HA resource kept on two nodes
  pco appliance convert --vmid 120 --ha --ha-nodes pve1,pve2

  # Then a join token for node pve2, made on the controller
  pct exec 120 -- pco cluster join-token --node pve2 --file /root/pve2.join

  # And on pve2, with the token copied there
  pco appliance install --kind agent --storage local-zfs --join-file /root/pve2.join --checksums checksums.txt`

func (a *app) applianceConvertCmd() *cobra.Command {
	var o applianceinstall.ConvertOptions
	cmd := &cobra.Command{
		Use:     "convert",
		Short:   "Make a combined appliance the controller of a cluster",
		Long:    convertLong,
		Example: convertExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			profile, err := store.DetectProfile(a.profileFile)
			if err != nil {
				return err
			}
			if profile == store.ProfileAppliance {
				if o.VMID != 0 || o.HA {
					return errors.New("inside the appliance, pco appliance convert takes no flags: run it on the node with --vmid")
				}
				return a.convertInside(cmd)
			}
			if o.VMID == 0 {
				return errors.New("--vmid names the combined appliance to convert")
			}
			p, err := a.prompter(cmd, o.Yes)
			if err != nil {
				return err
			}
			in := applianceinstall.New(setup.NewHostRunner(), p, a.now, rand.Reader, applianceinstall.JournalDir)
			return in.Convert(cmd.Context(), o)
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&o.VMID, "vmid", 0, "the VMID of the combined appliance")
	flags.BoolVarP(&o.Yes, "yes", "y", false, "ask nothing (needed without a terminal)")
	flags.BoolVar(&o.HA, "ha", false, "make the controller a Proxmox HA resource")
	flags.StringSliceVar(&o.HANodes, "ha-nodes", nil, "the nodes the HA controller is kept on, repeated or separated by commas")
	return cmd
}

// convertInside is the half of a convert inside the appliance, which the
// node's half runs: the kind in the store, and the daemon restarted.
func (a *app) convertInside(cmd *cobra.Command) error {
	if err := a.insideAppliance(cmd, "pco appliance convert runs on the node of a combined appliance"); err != nil {
		return err
	}
	paths, err := a.applianceVolume()
	if err != nil {
		return err
	}
	st, err := store.Open(paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	line, err := appliance.ConvertToController(cmd.Context(), st, connector.NewSystemctl())
	if err != nil {
		return err
	}
	out := &screen{w: cmd.OutOrStdout()}
	out.println(line)
	return out.done()
}
