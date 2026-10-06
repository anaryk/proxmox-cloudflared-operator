# pco network attach

Attach the network of a bridge or VLAN the node has no address in: a leg of pco's own with
the address `--address` gives, with the length of the guests' prefix, which nobody may hold.
The address is probed first and the result shown, then the question asked; a script passes
`--yes`. A bridge with a physical uplink needs `--confirm-uplink`: the leg's MAC becomes visible
on that LAN, so the address has to be one reserved for pco there. A prefix that overlaps a
route of the node on another interface is refused unless `--overlap-ok` says that the guests
there are meant to be reached through the leg; the network records it. The first network
enables pco-netns.service. The next cycle raises the leg, and the routes of the guests
there are served through it from then on.

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
```

## Flags

```text
    --address string   the address of the leg with the length of the guests' prefix, as 10.77.0.250/24 (required)
    --confirm-uplink   attach to a bridge with a physical uplink, where the leg's MAC is seen on the LAN
-h, --help             help for attach
    --overlap-ok       attach a prefix that overlaps a route of the node on another interface
-y, --yes              do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
