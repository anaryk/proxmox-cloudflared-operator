# pco network probe

Probe an address on a bridge or VLAN the way the leg of a network checks its own before it
raises it: an ARP probe from no address, through the leg when the network is attached and
through a port of the gateway's own otherwise, which is seen on the LAN of a bridge with a
physical uplink while it probes. Without `--prefix` the prefix is the one most guest addresses
seen there are in, a /24 around them; without `--address` it is the highest address of the
prefix that no guest is known to have. `--address` may carry the length of the prefix, as
10.77.0.250/24. The command exits with 1 when something answered.

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
```

## Flags

```text
    --address string   the address to probe, as 10.77.0.250 or 10.77.0.250/24
-h, --help             help for probe
    --prefix string    the prefix of the guests' network, as 10.77.0.0/24
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
