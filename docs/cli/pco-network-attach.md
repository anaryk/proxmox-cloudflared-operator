# pco network attach

Attach the network of a bridge or VLAN the node has no address in: a leg of pco's own with
the address `--address` gives, with the length of the guests' prefix, which nobody may hold.
The address is probed first and the result shown, then the question asked; a script passes
`--yes`. The appliance cannot probe a bridge before its card is there: it says so, and the leg
checks its address when the next cycle raises it and keeps it off if anyone answers. A
bridge with a physical uplink needs `--confirm-uplink`: the leg's MAC becomes visible
on that LAN, so the address has to be one reserved for pco there. A prefix that overlaps a
route of the node on another interface is refused unless `--overlap-ok` says that the guests
there are meant to be reached through the leg; the network records it. The first network
enables pco-netns.service. The next cycle raises the leg, and the routes of the guests
there are served through it from then on.

On a cluster the network has a scope, which the command asks for when `--scope` does not give
it, proposing the one the configuration of the bridge suggests. Of scope cluster, a bridge
with a physical uplink or a vnet of a VLAN, QinQ, VXLAN or EVPN zone, every node with the
bridge gets a leg: `--address` is this node's, `--leg` node=address names another node's, and
the others get the highest free address of the prefix. The agent of each node probes its
own and checks that the bridge is there; a node without the bridge, or whose agent does not
answer, gets no leg, and a later attach that names it adds one. Of scope node, an isolated
bridge or a vnet of a Simple zone, the network has one leg, on this node or the one `--node`
names, at `--address`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco network attach <bridge>[:<vlan>] --address A/len [flags]
```

## Examples

```text
# Attach vmbr1 with the leg at 10.77.0.250
pco network attach vmbr1 --address 10.77.0.250/24

# VLAN 20 of a bridge with a physical uplink, from a script
pco network attach vmbr2:20 --address 10.20.0.250/24 --confirm-uplink --yes

# A network that overlaps a route of the node, on purpose
pco network attach pcot1 --address 192.168.4.249/24 --overlap-ok

# On a cluster, VLAN 20 of an uplinked bridge on every node, pve2 at an address of its own
pco network attach vmbr2:20 --address 10.20.0.250/24 --scope cluster --leg pve2=10.20.0.249 --confirm-uplink

# On a cluster, an isolated bridge of pve3
pco network attach vmbr7 --address 10.70.0.250/24 --scope node --node pve3
```

## Flags

```text
    --address string    the address of the leg with the length of the guests' prefix, as 10.77.0.250/24
    --confirm-uplink    attach to a bridge with a physical uplink, where the leg's MAC is seen on the LAN
-h, --help              help for attach
    --leg stringArray   the leg of another node of a network of --scope cluster, as pve2=10.77.0.249 (repeatable)
    --node string       the node of a network of --scope node, this one when not given
    --overlap-ok        attach a prefix that overlaps a route of the node on another interface
    --scope string      on a cluster, cluster for one segment on every node or node for a bridge of one node
-y, --yes               do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
