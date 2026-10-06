package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// newNetworkManagedCmd is pco network managed, the managed network: the SDN
// vnet of pco's own that guests which consent get a card on.
func newNetworkManagedCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "managed",
		Short: "Make pco's managed network, the SDN vnet pconet",
		Long: "The managed network is an SDN vnet of pco's own, pconet in the zone pco of type simple,\n" +
			"without a subnet: pco's leg is its only gateway, and pco gives the guests that consent a\n" +
			"card on it with an address of managed.subnet. pco network managed enable makes it.",
		Example: "  # On a host: make the zone, the vnet and the network, without a question\n" +
			"  pco network managed enable --yes\n\n" +
			"  # In an appliance, once the node ran pco appliance managed enable\n" +
			"  pco network managed enable",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newNetworkManagedEnableCmd(a))
	return cmd
}

func newNetworkManagedEnableCmd(a *app) *cobra.Command {
	var o setup.ManagedOptions
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Make the managed network",
		Long: "Make the managed network. On a host it runs as root and does what pco setup --managed adds:\n" +
			"it takes the lock of the SDN configuration, refuses changes of others that are pending\n" +
			"unless --confirm-sdn-apply applies them along, makes the zone pco and the vnet pconet with\n" +
			"isolated ports where they are missing, asks before the apply, which reloads the network on\n" +
			"every node (--yes answers that), and stores the network. When it is the first network of\n" +
			"the node, it enables and starts pco-netns.service, which brings up the gateway namespace\n" +
			"the daemon puts the leg in.\n\n" +
			"In an appliance the node makes the vnet and grants it, with pco appliance managed enable\n" +
			"as root on the node; this command then asks the daemon to store the network, and the\n" +
			"appliance adds its own card on pconet.\n\n" +
			"A Proxmox VE without the lock of the SDN configuration reads the pending changes again\n" +
			"right before the apply. A lock that a run cut short left is released with pvesh delete\n" +
			"/cluster/sdn/lock --force.",
		Example: "  # On a host, asking before the apply\n" +
			"  pco network managed enable\n\n" +
			"  # On a host, applying changes of others that are pending as well\n" +
			"  pco network managed enable --confirm-sdn-apply --yes\n\n" +
			"  # Inside the appliance, after pco appliance managed enable --vmid 120 on the node\n" +
			"  pco network managed enable",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			profile, err := store.DetectProfile(a.profileFile)
			if err != nil {
				return err
			}
			if profile == store.ProfileAppliance {
				if o.ConfirmSDNApply {
					return errors.New("--confirm-sdn-apply is for a host: in the appliance the node makes and applies the SDN " +
						"with pco appliance managed enable")
				}
				if err := a.client().EnableManaged(cmd.Context()); err != nil {
					return a.explain(cmd.Context(), err)
				}
				s := &screen{w: cmd.OutOrStdout()}
				s.printf("The managed network is enabled: the appliance puts its card on %s in its next cycle.\n", setup.ManagedVnet)
				return s.done()
			}
			p, err := a.prompter(cmd, o.Yes)
			if err != nil {
				return err
			}
			s, err := a.nodeSetup(p)
			if err != nil {
				return err
			}
			return s.EnableManaged(cmd.Context(), o)
		},
	}
	flags := cmd.Flags()
	flags.BoolVarP(&o.Yes, "yes", "y", false, "apply the SDN configuration without asking (needed without a terminal)")
	flags.BoolVar(&o.ConfirmSDNApply, "confirm-sdn-apply", false, "apply the changes of others that are pending in the SDN along with pco's")
	return cmd
}
