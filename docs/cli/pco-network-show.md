# pco network show

Show a network, named by its id as pco network list shows it: the bridge and VLAN, the
prefix, the leg on each node with how the gateway last found it, its mark and MAC, whether
the bridge has a physical uplink, overlap-ok when it was attached over a route of the node,
the service addresses the connectors dial for the guests behind it, and the routes served
through it.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco network show <id> [flags]
```

## Examples

```text
# The network of vmbr1
pco network show vmbr1

# VLAN 20 of vmbr2, as JSON
pco network show vmbr2.20 --json
```

## Flags

```text
-h, --help   help for show
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
