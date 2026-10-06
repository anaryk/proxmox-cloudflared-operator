package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
		a.applianceNetworkCmd(),
		a.applianceManagedCmd(),
		a.appliancePurgeCmd(),
		a.applianceManifestCmd(),
		a.applianceAttachmentsCmd(),
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
	"node's name servers, the node's address the appliance reaches the API at (--api-host\n" +
	"when it has none on the bridge), the name the API's certificate verifies under, another\n" +
	"install of pco, and the principals other than the admins who could reach into the\n" +
	"appliance. Those are refused unless a NoAccess line is added for each, which only\n" +
	"--deny-access or a yes at the question does; --yes never does. Each line is shown with\n" +
	"what it takes first: one on / or /vms takes from its principal every privilege in the\n" +
	"cluster, or on every guest, that no line further down grants it. The lines added are\n" +
	"recorded in the appliance's manifest, and uninstall takes them back.\n\n" +
	"The appliance asks the name servers the node asks; where the node asks the stub of\n" +
	"systemd-resolved at 127.0.0.53, those systemd-resolved forwards to. A server on the node's\n" +
	"loopback is one the container does not reach: with no other, the installer refuses and\n" +
	"--nameserver names the servers.\n\n" +
	"The web interface of the appliance listens on the IPv4 address of net0 (--ip, or the lease\n" +
	"DHCP gives it), port 8643, and nowhere else; it serves IPv4 only. The daemon follows a new\n" +
	"address of net0. The installer prints the fingerprint of its certificate to compare at\n" +
	"the first visit.\n\n" +
	"The template is downloaded through Proxmox and checked against --checksums, the\n" +
	"checksums.txt of the release, unless --template names it. Every object made is noted in\n" +
	"a journal under /root/.pco-appliance-install first: a step that fails, and SIGINT, SIGTERM\n" +
	"or SIGHUP, take back what the run made, and a run that was killed is finished with\n" +
	"--resume <journal>, which takes the --checksums, --release-base and --template given\n" +
	"with it over those of the run. The Cloudflare token, from --cf-token-file, is optional\n" +
	"and can be added inside later. It runs as root on the node and exits 0 once the\n" +
	"appliance is installed, 1 otherwise."

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
	flags.StringSliceVar(&f.o.Nameservers, "nameserver", nil, "a name server of the appliance, up to 3, repeated or separated by commas "+
		"(default: those the node asks, behind systemd-resolved's stub the ones it forwards to)")
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
	"  volume is marked, the container goes into pool pco unless it is in another pool, the\n" +
	"  gate tags are registered, and pco appliance init adopts the install the Cloudflare token\n" +
	"  sees (--install-id when it sees several), with the manifest rebuilt from what pco made\n" +
	"  in Proxmox.\n\n" +
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
	cmd.Flags().Lookup("vmid").Usage = "the VMID of the appliance"
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
	"The SDN zone pco and vnet pconet that pco appliance managed enable made go last, under the\n" +
	"lock of the SDN configuration, as the manifest names them. While a guest has a card on\n" +
	"pconet it refuses before anything goes, as the manifest goes with the container;\n" +
	"--detach-managed has the running appliance list the cards it attached and removes them\n" +
	"first, with what it changed of the guests' firewalls.\n\n" +
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
	flags.BoolVar(&o.DetachManaged, "detach-managed", false, "remove the cards the appliance attached on the managed network first, so that its zone and vnet can go")
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
		RunE:    func(cmd *cobra.Command, _ []string) error { return a.grantNetwork(cmd, o) },
	}
	networkFlags(cmd, &o)
	return cmd
}

// grantNetwork is pco appliance grant-network, which network allow runs as
// well.
func (a *app) grantNetwork(cmd *cobra.Command, o applianceinstall.NetworkOptions) error {
	in, err := a.networkInstaller(cmd, o.Yes)
	if err != nil {
		return err
	}
	if a.env.grantNetwork != nil {
		return a.env.grantNetwork(cmd.Context(), in, o)
	}
	return in.GrantNetworkCommand(cmd.Context(), o)
}

// applianceNetworkCmd is pco appliance network, the short forms of the
// network grants.
func (a *app) applianceNetworkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Grant the appliance a bridge or vnet, in short",
		Long: "Grant the appliance a bridge or vnet, in short: pco appliance network allow is\n" +
			"pco appliance grant-network with the bridge and the VLAN in one argument.",
		Example: "  # Let the appliance attach a card to VLAN 20 of vmbr1\n" +
			"  pco appliance network allow vmbr1:20 --vmid 120\n\n" +
			"  # The whole of the plain bridge vmbr2, without a question\n" +
			"  pco appliance network allow vmbr2 --vmid 120 --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(a.applianceNetworkAllowCmd())
	return cmd
}

func (a *app) applianceNetworkAllowCmd() *cobra.Command {
	var o applianceinstall.NetworkOptions
	cmd := &cobra.Command{
		Use:   "allow <bridge>[:<vlan>]",
		Short: "Grant the appliance a bridge or vnet, or one VLAN of it",
		Long: "The same as pco appliance grant-network --bridge <bridge> [--vlan <vlan>]: it grants the\n" +
			"appliance the bridge or vnet, or the VLAN after the colon, with the lines, the question and\n" +
			"the record in the manifest of grant-network. It runs as root on the node and exits 0 once\n" +
			"the grant holds, 1 otherwise.",
		Example: "  # The plain bridge vmbr1\n" +
			"  pco appliance network allow vmbr1 --vmid 120\n\n" +
			"  # VLAN 20 of the VLAN-aware bridge vmbr2, without a question\n" +
			"  pco appliance network allow vmbr2:20 --vmid 120 --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bridge, vlan, err := parseBridgeVLAN(args[0])
			if err != nil {
				return err
			}
			o.Bridge, o.VLAN = bridge, vlan
			return a.grantNetwork(cmd, o)
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&o.VMID, "vmid", 0, "the VMID of the appliance")
	flags.BoolVarP(&o.Yes, "yes", "y", false, "do not ask (needed without a terminal)")
	_ = cmd.MarkFlagRequired("vmid")
	return cmd
}

// parseBridgeVLAN reads <bridge> or <bridge>:<vlan>.
func parseBridgeVLAN(s string) (string, int, error) {
	bridge, tag, tagged := strings.Cut(s, ":")
	if !tagged {
		return bridge, 0, nil
	}
	vlan, err := strconv.Atoi(tag)
	if err != nil || bridge == "" || vlan < 1 || vlan > 4094 {
		return "", 0, fmt.Errorf("%q: want <bridge> or <bridge>:<vlan>, the VLAN a number from 1 to 4094", s)
	}
	return bridge, vlan, nil
}

const managedEnableLong = "Make the managed network of an appliance on this node: pco's own SDN vnet pconet in the\n" +
	"zone pco of type simple, without a subnet and without isolated ports, as the appliance's own\n" +
	"card is a port of it and the guest firewall isolates the guests. First it reads the node's\n" +
	"firewall: it refuses the nftables backend (nftables: 1 in the node's firewall options), a\n" +
	"datacenter firewall that is off, and pve-firewall not running, each before anything is made.\n" +
	"Then it takes the lock of the SDN configuration, refuses changes of others that are pending\n" +
	"unless --confirm-sdn-apply applies them along, makes the zone and the vnet where they are\n" +
	"missing, each noted in the appliance's manifest first, and asks before the apply, which\n" +
	"reloads the network on every node. Last it grants the appliance the vnet as grant-network\n" +
	"does: role PCOManaged on the appliance and role PCOSDN on /sdn/zones/pco/pconet, for user\n" +
	"pco@pve and the appliance's token, and asks; --yes answers both questions.\n\n" +
	"--pool <name> grants role PCOManaged on /pool/<name> as well: the appliance's token may then\n" +
	"change the network cards and the firewall of every guest in that pool, so that automatic\n" +
	"consent attaches them without a pveum line per guest. It is never granted on /.\n\n" +
	"The appliance must run, as its manifest notes what is made. Inside it, pco network managed\n" +
	"enable then takes the network into the store. It runs as root on the node and exits 0 once\n" +
	"the vnet is there and granted, 1 otherwise."

const managedEnableExample = `  # Make the managed network of the appliance in container 120
  pco appliance managed enable --vmid 120

  # Without a question, and grant the cards of the guests in pool web too
  pco appliance managed enable --vmid 120 --pool web --yes`

// applianceManagedCmd is pco appliance managed, run on the node.
func (a *app) applianceManagedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "managed",
		Short: "Make the managed network of the appliance",
		Long: "Make the managed network of the appliance from the node: the SDN vnet pconet and the grants\n" +
			"the appliance needs to put cards on it. pco appliance managed enable does it.",
		Example: "  # Make the managed network of the appliance in container 120\n" +
			"  pco appliance managed enable --vmid 120\n\n" +
			"  # Then, inside the appliance, take it into the store\n" +
			"  pct exec 120 -- pco network managed enable",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return cmd.Help()
		},
	}
	var o applianceinstall.ManagedOptions
	enable := &cobra.Command{
		Use:     "enable",
		Short:   "Make the SDN vnet of the managed network and grant it to the appliance",
		Long:    managedEnableLong,
		Example: managedEnableExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := a.networkInstaller(cmd, o.Yes)
			if err != nil {
				return err
			}
			return in.ManagedEnableCommand(cmd.Context(), o)
		},
	}
	flags := enable.Flags()
	flags.IntVar(&o.VMID, "vmid", 0, "the VMID of the appliance")
	flags.StringVar(&o.Pool, "pool", "", "grant the appliance the network cards and the firewall of every guest in this pool as well")
	flags.BoolVar(&o.ConfirmSDNApply, "confirm-sdn-apply", false, "apply the changes of others that are pending in the SDN along with pco's")
	flags.BoolVarP(&o.Yes, "yes", "y", false, "apply and grant without asking (needed without a terminal)")
	_ = enable.MarkFlagRequired("vmid")
	cmd.AddCommand(enable)
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

// applianceAttachmentsCmd is what pco appliance uninstall --detach-managed
// runs inside the appliance to learn the cards it attached.
func (a *app) applianceAttachmentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "attachments",
		Short:  "List the cards the appliance attached on the managed network (run by uninstall)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.insideAppliance(cmd, "pco uninstall --detach-managed reads the store itself"); err != nil {
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
			have, err := st.Attachments()
			if err != nil {
				return fmt.Errorf("reading the attachments: %w", err)
			}
			list := make([]store.Attachment, 0, len(have))
			for _, owner := range slices.Sorted(maps.Keys(have)) {
				list = append(list, have[owner])
			}
			b, err := json.Marshal(list)
			if err != nil {
				return err
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("%s\n", b)
			return s.done()
		},
	}
}

// applianceManifestCmd changes the manifest of the appliance for pco
// appliance grant-network, revoke-network and managed enable, which run on
// the node.
func (a *app) applianceManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "manifest",
		Short:  "Record a network grant in the manifest of the appliance (run by grant-network)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(a.applianceManifestSDNCmd())
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
		sub.Flags().StringVar(&g.Pool, "pool", "", "the pool of a pool grant, in place of a network")
		sub.Flags().StringArrayVar(&roles, "created-role", nil, "a role the grant made")
		sub.MarkFlagsOneRequired("vnet", "pool")
		sub.MarkFlagsMutuallyExclusive("vnet", "pool")
		cmd.AddCommand(sub)
	}
	return cmd
}

// applianceManifestSDNCmd notes in the manifest of the appliance whether pco
// made the SDN zone and the vnet of the managed network, for pco appliance
// managed enable, which notes each before it makes it.
func (a *app) applianceManifestSDNCmd() *cobra.Command {
	var zone, vnet bool
	cmd := &cobra.Command{
		Use:   "sdn",
		Short: "note the SDN zone and vnet of the managed network",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.insideAppliance(cmd, "pco setup --managed notes them itself"); err != nil {
				return err
			}
			paths, err := a.applianceVolume()
			if err != nil {
				return err
			}
			var z, v *bool
			if cmd.Flags().Changed("created-zone") {
				z = &zone
			}
			if cmd.Flags().Changed("created-vnet") {
				v = &vnet
			}
			return setup.NoteManagedObjects(paths.Local, z, v)
		},
	}
	cmd.Flags().BoolVar(&zone, "created-zone", false, "pco made the SDN zone pco")
	cmd.Flags().BoolVar(&vnet, "created-vnet", false, "pco made the SDN vnet pconet")
	cmd.MarkFlagsOneRequired("created-zone", "created-vnet")
	return cmd
}
