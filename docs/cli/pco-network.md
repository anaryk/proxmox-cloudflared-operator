# pco network

A guest on a bridge or VLAN the node has an address in is reached directly. One on a bridge
or VLAN the node is not on is reached through a leg of pco's own, which attaching the network
puts there: a network card of the gateway namespace (on the host) or of the appliance, with
an address of the guests' network, and every target behind it published under a service
address of 198.18.0.0/16. Nothing is written to `/etc/network/interfaces` and the
guests are not touched. A bridge and VLAN to attach to is written as vmbr1 or vmbr1:20; the
network attached there has the id vmbr1 or vmbr1.20.

## Usage

```text
pco network [command]
```

## Examples

```text
# The bridges and VLANs a network may be attached to, with the guests seen there
pco network candidates

# Attach vmbr1 with the leg at 10.77.0.250
pco network attach vmbr1 --address 10.77.0.250/24

# The networks attached
pco network list
```

## Commands

- [pco network attach](pco-network-attach.md): Attach a network: a leg of pco's own into a bridge or VLAN
- [pco network candidates](pco-network-candidates.md): List the bridges and VLANs of the node a network may be attached to
- [pco network detach](pco-network-detach.md): Detach a network: remove its leg and its service addresses
- [pco network list](pco-network-list.md): List the networks attached, with their legs and the routes served through them
- [pco network probe](pco-network-probe.md): Ask who answers for an address on a bridge or VLAN
- [pco network show](pco-network-show.md): Show a network with its leg, its service addresses and the routes served through it

## Flags

```text
-h, --help   help for network
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
