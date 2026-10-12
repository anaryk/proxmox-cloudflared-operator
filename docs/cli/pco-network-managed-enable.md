# pco network managed enable

Make the managed network. On a host it runs as root and does what pco setup `--managed` adds:
it takes the lock of the SDN configuration, refuses changes of others that are pending
unless `--confirm-sdn-apply` applies them along, makes the zone pco and the vnet pconet with
isolated ports where they are missing, asks before the apply, which reloads the network on
every node (`--yes` answers that), and stores the network: through the daemon while it runs,
itself otherwise. Before anything is made, managed.subnet is checked against the routes of
the nodes, the addresses of the guests and the attached networks. When it is the first
network of the node, it enables and starts pco-netns.service, which brings up the gateway
namespace the daemon puts the leg in.

In an appliance the node makes the vnet and grants it, with pco appliance managed enable
as root on the node; this command then asks the daemon to store the network, and the
appliance adds its own card on pconet.

A Proxmox VE without the lock of the SDN configuration reads the pending changes again
right before the apply. A lock that a run cut short left is released with pvesh delete
`/cluster/sdn/lock` `--force`.

## Usage

```text
pco network managed enable [flags]
```

## Examples

```text
# On a host, asking before the apply
pco network managed enable

# On a host, applying changes of others that are pending as well
pco network managed enable --confirm-sdn-apply --yes

# Inside the appliance, after pco appliance managed enable --vmid 120 on the node
pco network managed enable
```

## Flags

```text
    --confirm-sdn-apply   apply the changes of others that are pending in the SDN along with pco's
-h, --help                help for enable
-y, --yes                 apply the SDN configuration without asking (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network managed](pco-network-managed.md): Make pco's managed network, the SDN vnet pconet
- [Command reference](index.md): every command of pco, by group
