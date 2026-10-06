package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/applianceinstall"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// applianceNodeCmds are the commands of the appliance run on the node:
// those that install, repair and remove it, and grant it networks.
func (a *app) applianceNodeCmds() []*cobra.Command {
	return []*cobra.Command{
		a.applianceInstallCmd(),
		a.applianceRepairCmd(),
		a.applianceUninstallCmd(),
		a.applianceGrantCmd(),
		a.applianceRevokeCmd(),
		a.appliancePurgeCmd(),
		a.applianceManifestCmd(),
	}
}

// installFlags are the flags of pco appliance install and repair.
type installFlags struct {
	o                applianceinstall.Options
	tokenFile        string
	noTags, keepTmpl bool
}

const installLong = "Install the pco appliance on this Proxmox VE node: an unprivileged Debian container that\n" +
	"runs pco and its connectors, with its state on a volume of its own that backups leave\n" +
	"out, in pool pco, protected, started at boot. It makes role PCO, user pco@pve and a\n" +
	"privilege-separated API token pco@pve!vm<vmid> for the appliance, registers the gate tags,\n" +
	"pushes the cluster CA and a bootstrap with the token into the container and has pco\n" +
	"appliance init make its store; nothing is installed on the node itself.\n\n" +
	"First it looks: the version of Proxmox VE, the storage, the bridge and its VLANs, the\n" +
	"node's address the appliance reaches the API at (--api-host when it has none on the\n" +
	"bridge), the name the API's certificate verifies under, another install of pco, and the\n" +
	"principals other than the admins who could reach into the appliance. Those are refused\n" +
	"unless a NoAccess line is added for each, which only --deny-access or a yes at the\n" +
	"question does; --yes never does. Each line is shown with what it takes first: one on /\n" +
	"or /vms takes from its principal every privilege in the cluster, or on every guest, that\n" +
	"no line further down grants it. The lines added are recorded in the appliance's\n" +
	"manifest, and uninstall takes them back.\n\n" +
	"The template is downloaded through Proxmox and checked against --checksums, the\n" +
	"checksums.txt of the release, unless --template names it. Every object made is noted in\n" +
	"a journal under /root/.pco-appliance-install first: a step that fails, and SIGINT, SIGTERM\n" +
	"or SIGHUP, take back what the run made, and a run that was killed is finished with\n" +
	"--resume <journal>. The Cloudflare token, from --cf-token-file, is optional and can be\n" +
	"added inside later. It runs as root on the node and exits 0 once the appliance is\n" +
	"installed, 1 otherwise."

const installExample = `  # Install with the defaults: the next free VMID, DHCP on vmbr0
  pco appliance install --storage local-zfs --checksums checksums.txt

  # On a VLAN-aware vmbr0, in the untagged VLAN the node's own address is in
  pco appliance install --storage local-zfs --vlan 1 --checksums checksums.txt

  # A static address on VLAN 20 of a VLAN-aware bridge, and the first Cloudflare token
  pco appliance install --storage local-zfs --bridge vmbr1 --vlan 20 --ip 192.0.2.120/24,gw=192.0.2.1 --vmid 120 --cf-token-file /root/cf-token

  # Offline, from a template file, without a question
  pco appliance install --storage local-zfs --template /root/pco-appliance_1.4.0_amd64.tar.zst --yes

  # Finish a run that was killed
  pco appliance install --resume /root/.pco-appliance-install/20261006T101500-3f2a.json`

func (a *app) applianceInstallCmd() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:     "install",
		Short:   "Install the pco appliance on this node",
		Long:    installLong,
		Example: installExample,
		Args:    tokenArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			in, o, err := a.nodeInstaller(cmd, &f)
			if err != nil {
				return err
			}
			return in.Install(cmd.Context(), o)
		},
	}
	flags := cmd.Flags()
	a.commonInstallFlags(cmd, &f)
	flags.StringVar(&f.o.TemplateStorage, "template-storage", "", "the storage the template is downloaded to (default: --storage when it holds templates, else the one storage that does)")
	flags.StringVar(&f.o.Bridge, "bridge", applianceinstall.DefaultBridge, "the bridge of the appliance's card")
	flags.IntVar(&f.o.VLAN, "vlan", 0, "the VLAN of the card; a VLAN-aware bridge needs one, its PVID (1 unless bridge-pvid says otherwise) "+
		"for the untagged VLAN the node's own address is in")
	flags.StringVar(&f.o.IP, "ip", applianceinstall.DefaultIP, "dhcp, or the card's address with its prefix and an optional gateway, as 192.0.2.120/24,gw=192.0.2.1")
	flags.IntVar(&f.o.Cores, "cores", applianceinstall.DefaultCores, "the cores of the container")
	flags.IntVar(&f.o.MemoryMB, "memory", applianceinstall.DefaultMemoryMB, "the memory of the container, in MB")
	flags.IntVar(&f.o.RootFSGB, "rootfs-size", applianceinstall.DefaultRootFSGB, "the size of the root filesystem, in GiB")
	flags.StringVar(&f.o.Template, "template", "", "the template as a file, by its absolute path (default: downloaded through Proxmox)")
	flags.StringVar(&f.o.ReleaseBase, "release-base", "", "where the template is downloaded from (default: the GitHub release of this version)")
	flags.StringVar(&f.o.ChecksumsFile, "checksums", "", "the checksums.txt of the release, which the template is checked against")
	flags.BoolVar(&f.keepTmpl, "keep-template", true, "keep a template this run downloaded when the run is taken back")
	flags.BoolVar(&f.o.DenyAccess, "deny-access", false, "add the NoAccess lines shown, those on / or /vms too, for the principals that could reach into the appliance, instead of refusing")
	flags.StringVar(&f.o.Resume, "resume", "", "finish the run of this journal, or take it back")
	return cmd
}

// commonInstallFlags are the flags install and repair share.
func (a *app) commonInstallFlags(cmd *cobra.Command, f *installFlags) {
	flags := cmd.Flags()
	flags.BoolVarP(&f.o.Yes, "yes", "y", false, "take the default answers and ask nothing (needed without a terminal); never adds NoAccess lines")
	flags.IntVar(&f.o.VMID, "vmid", 0, "the VMID of the appliance (default: the next free one)")
	flags.StringVar(&f.o.Storage, "storage", "", "the storage of the container and its state volume (default: the one storage that holds containers)")
	flags.IntVar(&f.o.StateGB, "state-size", applianceinstall.DefaultStateGB, "the size of the state volume, in GiB")
	flags.StringVar(&f.o.APIHost, "api-host", "", "the node's address the appliance reaches the API at (default: its address on the bridge)")
	flags.StringVar(&f.o.APICA, "api-ca", "", "a CA file for an API certificate that neither the cluster CA nor the system roots verify")
	flags.StringVar(&f.tokenFile, "cf-token-file", "", "read a Cloudflare API token from this file")
	flags.BoolVar(&f.noTags, "no-registered-tags", false, "do not register the gate tags, which lets whoever may edit a guest set them")
	flags.StringVar(&f.o.GateTag, "gate-tag", "", "the gate tag of the appliance (default cf-tunnel)")
}

// tokenArgs refuses arguments without repeating them: a token typed where a
// flag value belongs is one.
func tokenArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%q takes no arguments: the Cloudflare token is read from --cf-token-file", cmd.CommandPath())
	}
	return nil
}

// nodeInstaller is the installer of this node and the options of the flags.
// What it can check without the node it checks first: the Cloudflare API of
// tests, and that this is no appliance.
func (a *app) nodeInstaller(cmd *cobra.Command, f *installFlags) (*applianceinstall.Installer, applianceinstall.Options, error) {
	o := f.o
	override, err := a.cloudflareOverride()
	if err != nil {
		return nil, o, err
	}
	if err := a.onTheNode(cmd); err != nil {
		return nil, o, err
	}
	if f.noTags {
		no := false
		o.RegisterTags = &no
	}
	o.KeepTemplate = f.keepTmpl
	if f.tokenFile != "" {
		if o.CloudflareToken, err = a.tokenOfFile(cmd, f.tokenFile); err != nil {
			return nil, o, err
		}
	}
	p, err := a.prompter(cmd, o.Yes)
	if err != nil {
		return nil, o, err
	}
	if override != "" {
		p.Warn("%s", overrideLine(override))
		o.CloudflareAPI = override
	}
	return applianceinstall.New(setup.NewHostRunner(), p, a.now, rand.Reader, applianceinstall.JournalDir), o, nil
}

// onTheNode refuses a command of the node inside an appliance.
func (a *app) onTheNode(cmd *cobra.Command) error {
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile == store.ProfileAppliance {
		return fmt.Errorf("%s runs on the Proxmox VE node, not inside the appliance", cmd.CommandPath())
	}
	return nil
}

func (a *app) tokenOfFile(cmd *cobra.Command, path string) (string, error) {
	raw, loose, err := readTokenFile(path)
	if loose {
		s := &screen{w: cmd.ErrOrStderr()}
		s.printf("warning: %s can be read by others; restrict it with chmod 600\n", path)
	}
	if err != nil {
		return "", fmt.Errorf("reading the Cloudflare token: %w", err)
	}
	token := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(token) == "" {
		return "", errors.New("the Cloudflare token is empty")
	}
	return token, nil
}

const repairLong = "Put the appliance right on this node after a restore, after the certificate of the API\n" +
	"changed, or when its token was lost. It starts the container if it is stopped and stops it\n" +
	"again at the end. Its state volume decides how:\n\n" +
	"  with the state of an install, the API token is made anew, the gate tags and the\n" +
	"  certificate are checked again (--api-ca for a certificate of a private CA), and pco\n" +
	"  appliance init repairs the store with the token, the MACs, the endpoint and the\n" +
	"  node's addresses as they are now;\n" +
	"  without state, only with --recover (a restore to another VMID, a restore over the\n" +
	"  appliance, a lost volume): the container gets a state volume again if it has none, the\n" +
	"  volume is marked, and pco appliance init adopts the install the Cloudflare token sees\n" +
	"  (--install-id when it sees several), with the manifest rebuilt from what pco made in\n" +
	"  Proxmox.\n\n" +
	"A copy of the appliance beside the one it was made from, a clone or a restore while the\n" +
	"original is still there, is refused: it would write the install of the original with its\n" +
	"credentials. Remove the copy, or install an appliance anew.\n\n" +
	"A repair takes nothing back when it fails; running it again finishes it. It runs as root\n" +
	"on the node that has the container; on another node of the cluster it refuses and names\n" +
	"that node. It exits 0 once the appliance is repaired, 1 otherwise."

const repairExample = `  # After the cluster CA was made anew
  pco appliance repair --vmid 120

  # The API now presents a certificate of a private CA
  pco appliance repair --vmid 120 --api-ca /root/example-ca.pem

  # A restore to VMID 121 with an empty state volume
  pco appliance repair --vmid 121 --recover --cf-token-file /root/cf-token`

func (a *app) applianceRepairCmd() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:     "repair",
		Short:   "Repair the appliance after a restore or a changed certificate",
		Long:    repairLong,
		Example: repairExample,
		Args:    tokenArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			f.keepTmpl = true
			in, o, err := a.nodeInstaller(cmd, &f)
			if err != nil {
				return err
			}
			return in.Repair(cmd.Context(), o.VMID, o)
		},
	}
	a.commonInstallFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.o.Recover, "recover", false, "the volume holds no state: adopt the install the Cloudflare token sees")
	cmd.Flags().StringVar(&f.o.InstallID, "install-id", "", "with --recover: the install to adopt, when the token sees several")
	_ = cmd.MarkFlagRequired("vmid")
	return cmd
}

const uninstallLong = "Remove the appliance and what the installer made for it from this node, as the marks\n" +
	"of the objects and the manifest in the appliance name them: the container with its state\n" +
	"volume, its token, its network grants, the NoAccess lines the installer added while they\n" +
	"are as it made them, and user pco@pve, role PCO and pool pco when nothing else uses them,\n" +
	"the gate tags the installer registered, and with --keep-template=false the template it\n" +
	"downloaded for the appliance, which the manifest names. It lists what goes and what\n" +
	"stays, and why, and asks once; --yes answers that.\n\n" +
	"What the install has at Cloudflare is deleted through the running appliance with\n" +
	"--purge-cloudflare, left with --keep-cloudflare, or asked about; with --yes, an install\n" +
	"with something at Cloudflare needs one of the two, as the credentials that reach it go\n" +
	"with the container. A copy of the appliance beside its original leaves Cloudflare alone,\n" +
	"as what it reaches there is the original's.\n\n" +
	"A part that fails is reported and the rest goes on; running it again finishes it. It\n" +
	"runs as root on the node that has the container; on another node of the cluster it\n" +
	"refuses and names that node, and only a container no node has counts as gone, whose\n" +
	"leftovers it removes. It exits 0 once the appliance is removed, 1 otherwise."

const uninstallExample = `  # Remove the appliance, asking about everything
  pco appliance uninstall --vmid 120

  # Without a question, deleting its DNS records and tunnel at Cloudflare
  pco appliance uninstall --vmid 120 --yes --purge-cloudflare

  # Without a question, leaving Cloudflare as it is, and removing the template too
  pco appliance uninstall --vmid 120 --yes --keep-cloudflare --keep-template=false`

func (a *app) applianceUninstallCmd() *cobra.Command {
	var (
		vmid int
		o    applianceinstall.UninstallOptions
	)
	cmd := &cobra.Command{
		Use:     "uninstall",
		Short:   "Remove the appliance from this node",
		Long:    uninstallLong,
		Example: uninstallExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			override, err := a.cloudflareOverride()
			if err != nil {
				return err
			}
			if err := a.onTheNode(cmd); err != nil {
				return err
			}
			p, err := a.prompter(cmd, o.Yes)
			if err != nil {
				return err
			}
			if override != "" {
				p.Warn("%s", overrideLine(override))
				o.CloudflareAPI = override
			}
			in := applianceinstall.New(setup.NewHostRunner(), p, a.now, rand.Reader, applianceinstall.JournalDir)
			return in.Uninstall(cmd.Context(), vmid, o)
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&vmid, "vmid", 0, "the VMID of the appliance")
	flags.BoolVarP(&o.Yes, "yes", "y", false, "remove the appliance without asking (needed without a terminal); with something at "+
		"Cloudflare, --purge-cloudflare or --keep-cloudflare as well")
	flags.BoolVar(&o.PurgeCloudflare, "purge-cloudflare", false, "delete the DNS records and the tunnel of the install at Cloudflare, through the appliance")
	flags.BoolVar(&o.KeepCloudflare, "keep-cloudflare", false, "leave the DNS records and the tunnel of the install at Cloudflare as they are")
	flags.BoolVar(&o.KeepTemplate, "keep-template", true, "keep the template the installer downloaded for the appliance; another template of pco always stays")
	cmd.MarkFlagsMutuallyExclusive("purge-cloudflare", "keep-cloudflare")
	_ = cmd.MarkFlagRequired("vmid")
	return cmd
}

const grantLong = "Grant the appliance a network its cards may be attached to: role PCOManaged\n" +
	"(VM.Config.Network) on the appliance and role PCOSDN (SDN.Use) on the network, the most\n" +
	"specific path of it (/sdn/zones/<zone>/<vnet> or /sdn/zones/<zone>/<vnet>/<vlan>; zone\n" +
	"localnetwork for a plain Linux bridge), each for user pco@pve and for the appliance's\n" +
	"token, which holds only what its user holds as well. A whole zone is never granted. The\n" +
	"roles are made when missing. It prints the lines and asks; --yes answers that. It warns\n" +
	"when the bridge carries an address of this node. The grant is recorded in the appliance's\n" +
	"manifest, and uninstall takes it back. It runs as root on the node and exits 0 once the\n" +
	"grant holds, 1 otherwise."

const grantExample = `  # Let the appliance attach a card to the plain bridge vmbr1
  pco appliance grant-network --vmid 120 --bridge vmbr1

  # VLAN 20 of the VLAN-aware bridge vmbr2, without a question
  pco appliance grant-network --vmid 120 --bridge vmbr2 --vlan 20 --yes`

func (a *app) applianceGrantCmd() *cobra.Command {
	var o applianceinstall.NetworkOptions
	cmd := &cobra.Command{
		Use:     "grant-network",
		Short:   "Grant the appliance a bridge or vnet to attach cards to",
		Long:    grantLong,
		Example: grantExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := a.networkInstaller(cmd, o.Yes)
			if err != nil {
				return err
			}
			return in.GrantNetworkCommand(cmd.Context(), o)
		},
	}
	networkFlags(cmd, &o)
	return cmd
}

const revokeLong = "Take back the grant of a network from the appliance: the lines of its token, those of\n" +
	"user pco@pve unless another token of it holds the same grant, and roles PCOManaged and\n" +
	"PCOSDN once no line names them. It says what it takes back and asks; --yes answers that.\n" +
	"It runs as root on the node and exits 0 once the grant is gone, 1 otherwise."

const revokeExample = `  # Take back the bridge vmbr1
  pco appliance revoke-network --vmid 120 --bridge vmbr1

  # VLAN 20 of vmbr2, without a question
  pco appliance revoke-network --vmid 120 --bridge vmbr2 --vlan 20 --yes`

func (a *app) applianceRevokeCmd() *cobra.Command {
	var o applianceinstall.NetworkOptions
	cmd := &cobra.Command{
		Use:     "revoke-network",
		Short:   "Take back the grant of a bridge or vnet from the appliance",
		Long:    revokeLong,
		Example: revokeExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := a.networkInstaller(cmd, o.Yes)
			if err != nil {
				return err
			}
			return in.RevokeNetworkCommand(cmd.Context(), o)
		},
	}
	networkFlags(cmd, &o)
	return cmd
}

func networkFlags(cmd *cobra.Command, o *applianceinstall.NetworkOptions) {
	flags := cmd.Flags()
	flags.IntVar(&o.VMID, "vmid", 0, "the VMID of the appliance")
	flags.StringVar(&o.Bridge, "bridge", "", "a Linux bridge of this node, or an SDN vnet")
	flags.IntVar(&o.VLAN, "vlan", 0, "a VLAN of a VLAN-aware bridge or vnet")
	flags.BoolVarP(&o.Yes, "yes", "y", false, "do not ask (needed without a terminal)")
	_ = cmd.MarkFlagRequired("vmid")
	_ = cmd.MarkFlagRequired("bridge")
}

func (a *app) networkInstaller(cmd *cobra.Command, yes bool) (*applianceinstall.Installer, error) {
	if err := a.noJSON(cmd); err != nil {
		return nil, err
	}
	if err := a.onTheNode(cmd); err != nil {
		return nil, err
	}
	p, err := a.prompter(cmd, yes)
	if err != nil {
		return nil, err
	}
	return applianceinstall.New(setup.NewHostRunner(), p, a.now, rand.Reader, applianceinstall.JournalDir), nil
}

// appliancePurgeCmd is what pco appliance uninstall runs inside the
// appliance to reach Cloudflare with the credentials stored there.
func (a *app) appliancePurgeCmd() *cobra.Command {
	var list bool
	cmd := &cobra.Command{
		Use:    "purge",
		Short:  "Delete what the install has at Cloudflare (run by pco appliance uninstall)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			override, err := a.cloudflareOverride()
			if err != nil {
				return err
			}
			if err := a.insideAppliance(cmd, "pco uninstall --purge-cloudflare deletes what an install has at Cloudflare"); err != nil {
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
			p := a.newPrompter(cmd, 0, false)
			if override != "" && !list {
				p.Warn("%s", overrideLine(override))
			}
			return setup.New(setup.NewHostRunner(), p, st, cloudflareClients(override), a.now, rand.Reader).Purge(cmd.Context(), list)
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "only name what would be deleted, one object a line")
	return cmd
}

// applianceManifestCmd changes the manifest of the appliance for pco
// appliance grant-network and revoke-network, which run on the node.
func (a *app) applianceManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "manifest",
		Short:  "Record a network grant in the manifest of the appliance (run by grant-network)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	for _, verb := range []string{"add", "remove"} {
		var (
			g     setup.NetworkGrant
			roles []string
		)
		sub := &cobra.Command{
			Use:   verb,
			Short: verb + " a network grant",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := a.noJSON(cmd); err != nil {
					return err
				}
				if err := a.insideAppliance(cmd, "pco appliance grant-network records its grants itself"); err != nil {
					return err
				}
				paths, err := a.applianceVolume()
				if err != nil {
					return err
				}
				g.CreatedRoles = roles
				if verb == "add" {
					return setup.AddNetworkGrant(paths.Local, g)
				}
				return setup.RemoveNetworkGrant(paths.Local, g)
			},
		}
		sub.Flags().StringVar(&g.Zone, "zone", "", "the SDN zone, localnetwork for a Linux bridge")
		sub.Flags().StringVar(&g.VNet, "vnet", "", "the bridge or vnet")
		sub.Flags().IntVar(&g.VLAN, "vlan", 0, "the VLAN, if any")
		sub.Flags().StringArrayVar(&roles, "created-role", nil, "a role the grant made")
		_ = sub.MarkFlagRequired("zone")
		_ = sub.MarkFlagRequired("vnet")
		cmd.AddCommand(sub)
	}
	return cmd
}
