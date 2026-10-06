# pco appliance revoke-network

Take back the grant of a network from the appliance: the lines of its token, those of
user pco@pve unless another token of it holds the same grant, and roles PCOManaged and
PCOSDN once no line names them. It says what it takes back and asks; `--yes` answers that.
It runs as root on the node and exits 0 once the grant is gone, 1 otherwise.

## Usage

```text
pco appliance revoke-network [flags]
```

## Examples

```text
# Take back the bridge vmbr1
pco appliance revoke-network --vmid 120 --bridge vmbr1

# VLAN 20 of vmbr2, without a question
pco appliance revoke-network --vmid 120 --bridge vmbr2 --vlan 20 --yes
```

## Flags

```text
    --bridge string   a Linux bridge of this node, or an SDN vnet
-h, --help            help for revoke-network
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
