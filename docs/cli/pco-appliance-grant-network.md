# pco appliance grant-network

Grant the appliance a network its cards may be attached to: role PCOManaged
(VM.Config.Network) on the appliance and role PCOSDN (SDN.Use) on the network, the most
specific path of it (`/sdn/zones/<zone>/<vnet>` or `/sdn/zones/<zone>/<vnet>/<vlan>`; zone
localnetwork for a plain Linux bridge), each for user pco@pve and for the appliance's
token, which holds only what its user holds as well. A whole zone is never granted. The
roles are made when missing. It prints the lines and asks; `--yes` answers that. It warns
when the bridge carries an address of this node. The grant is recorded in the appliance's
manifest, and uninstall takes it back. It runs as root on the node and exits 0 once the
grant holds, 1 otherwise.

## Usage

```text
pco appliance grant-network [flags]
```

## Examples

```text
# Let the appliance attach a card to the plain bridge vmbr1
pco appliance grant-network --vmid 120 --bridge vmbr1

# VLAN 20 of the VLAN-aware bridge vmbr2, without a question
pco appliance grant-network --vmid 120 --bridge vmbr2 --vlan 20 --yes
```

## Flags

```text
    --bridge string   a Linux bridge of this node, or an SDN vnet
-h, --help            help for grant-network
    --vlan int        a VLAN of a VLAN-aware bridge or vnet
    --vmid int        the VMID of the appliance
-y, --yes             do not ask (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
