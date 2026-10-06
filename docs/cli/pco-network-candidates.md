# pco network candidates

List the bridges and SDN vnets of the node, and the VLANs of them that guests or the node
are on: whether one has a physical uplink, how many guest addresses are seen there, static
or reported, the prefix they suggest, and whether a network may be attached there: not
where the node has an address (it reaches the guests directly), not where one is attached
already, and not where the profile cannot, as pco's managed network or, in the appliance, a
tag on a bridge that does not filter VLANs. The daemon reads Proxmox and lists the guests
for it.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped. It has every guest address by guest.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco network candidates [flags]
```

## Examples

```text
# The bridges and VLANs of the node
pco network candidates

# With the guest addresses seen on each, as JSON
pco network candidates --json
```

## Flags

```text
-h, --help   help for candidates
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
