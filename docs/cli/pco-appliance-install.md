# pco appliance install

Install the pco appliance on this Proxmox VE node: an unprivileged Debian container that
runs pco and its connectors, with its state on a volume of its own that backups leave
out, in pool pco, protected, started at boot. It makes role PCO, user pco@pve and a
privilege-separated API token `pco@pve!vm<vmid>` for the appliance, registers the gate tags,
pushes the cluster CA and a bootstrap with the token into the container and has pco
appliance init make its store; nothing is installed on the node itself.

First it looks: the version of Proxmox VE, the storage, the bridge and its VLANs, the
node's address the appliance reaches the API at (`--api-host` when it has none on the
bridge), the name the API's certificate verifies under, another install of pco, and the
principals other than the admins who could reach into the appliance. Those are refused
unless a NoAccess line is added for each, which only `--deny-access` or a yes at the
question does; `--yes` never does. Each line is shown with what it takes first: one on /
or /vms takes from its principal every privilege in the cluster, or on every guest, that
no line further down grants it. The lines added are recorded in the appliance's
manifest, and uninstall takes them back.

The template is downloaded through Proxmox and checked against `--checksums`, the
checksums.txt of the release, unless `--template` names it. Every object made is noted in
a journal under `/root/.pco-appliance-install` first: a step that fails, and SIGINT, SIGTERM
or SIGHUP, take back what the run made, and a run that was killed is finished with
`--resume` `<journal>`. The Cloudflare token, from `--cf-token-file`, is optional and can be
added inside later. It runs as root on the node and exits 0 once the appliance is
installed, 1 otherwise.

## Usage

```text
pco appliance install [flags]
```

## Examples

```text
# Install with the defaults: the next free VMID, DHCP on vmbr0
pco appliance install --storage local-zfs --checksums checksums.txt

# On a VLAN-aware vmbr0, in the untagged VLAN the node's own address is in
pco appliance install --storage local-zfs --vlan 1 --checksums checksums.txt

# A static address on VLAN 20 of a VLAN-aware bridge, and the first Cloudflare token
pco appliance install --storage local-zfs --bridge vmbr1 --vlan 20 --ip 192.0.2.120/24,gw=192.0.2.1 --vmid 120 --cf-token-file /root/cf-token

# Offline, from a template file, without a question
pco appliance install --storage local-zfs --template /root/pco-appliance_1.4.0_amd64.tar.zst --yes

# Finish a run that was killed
pco appliance install --resume /root/.pco-appliance-install/20261006T101500-3f2a.json
```

## Flags

```text
    --api-ca string             a CA file for an API certificate that neither the cluster CA nor the system roots verify
    --api-host string           the node's address the appliance reaches the API at (default: its address on the bridge)
    --bridge string             the bridge of the appliance's card (default "vmbr0")
    --cf-token-file string      read a Cloudflare API token from this file
    --checksums string          the checksums.txt of the release, which the template is checked against
    --cores int                 the cores of the container (default 1)
    --deny-access               add the NoAccess lines shown, those on / or /vms too, for the principals that could reach into the appliance, instead of refusing
    --gate-tag string           the gate tag of the appliance (default cf-tunnel)
-h, --help                      help for install
    --ip string                 dhcp, or the card's address with its prefix and an optional gateway, as 192.0.2.120/24,gw=192.0.2.1 (default "dhcp")
    --keep-template             keep a template this run downloaded when the run is taken back (default true)
    --memory int                the memory of the container, in MB (default 768)
    --no-registered-tags        do not register the gate tags, which lets whoever may edit a guest set them
    --release-base string       where the template is downloaded from (default: the GitHub release of this version)
    --resume string             finish the run of this journal, or take it back
    --rootfs-size int           the size of the root filesystem, in GiB (default 4)
    --state-size int            the size of the state volume, in GiB (default 1)
    --storage string            the storage of the container and its state volume (default: the one storage that holds containers)
    --template string           the template as a file, by its absolute path (default: downloaded through Proxmox)
    --template-storage string   the storage the template is downloaded to (default: --storage when it holds templates, else the one storage that does)
    --vlan int                  the VLAN of the card; a VLAN-aware bridge needs one, its PVID (1 unless bridge-pvid says otherwise) for the untagged VLAN the node's own address is in
    --vmid int                  the VMID of the appliance (default: the next free one)
-y, --yes                       take the default answers and ask nothing (needed without a terminal); never adds NoAccess lines
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
