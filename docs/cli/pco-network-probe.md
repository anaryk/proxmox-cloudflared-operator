# pco network probe

Probe an address on a bridge or VLAN the way the leg of a network checks its own before it
raises it: an ARP probe from no address, through the leg when the network is attached and
through a port of the gateway's own otherwise, which is seen on the LAN of a bridge with a
physical uplink while it probes. Without `--prefix` the prefix is the one most guest addresses
seen there are in, a /24 around them; without `--address` it is the highest address of the
prefix that no guest is known to have. `--address` may carry the length of the prefix, as
10.77.0.250/24. The appliance probes a bridge only through the card of a network attached
there; before that it says so, and the leg checks its address when it is raised. The command
exits with 1 when something answered or the address was not probed.

On a cluster `--scope` probes the legs of a network of that scope, as pco network attach does:
of scope cluster, this node's at `--address` and every other node's at the address `--leg` gives
or the highest free one, each by the agent of its node; of scope node, the leg of this node
or of the node `--node` names. A node without the bridge, or whose agent does not answer, is
shown without a leg and is no failure.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco network probe <bridge>[:<vlan>] [flags]
```

## Examples

```text
# Propose a free address on vmbr1
pco network probe vmbr1

# Is 10.20.0.250 free on VLAN 20 of vmbr2?
pco network probe vmbr2:20 --address 10.20.0.250/24

# On a cluster, the legs of VLAN 20 of vmbr2 on every node
pco network probe vmbr2:20 --scope cluster
```

## Flags

```text
    --address string    the address to probe, as 10.77.0.250 or 10.77.0.250/24
-h, --help              help for probe
    --leg stringArray   the leg of another node of a network of --scope cluster, as pve2=10.77.0.249 (repeatable)
    --node string       the node of a network of --scope node, this one when not given
    --prefix string     the prefix of the guests' network, as 10.77.0.0/24
    --scope string      on a cluster, probe the legs of a network of scope cluster or node
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
