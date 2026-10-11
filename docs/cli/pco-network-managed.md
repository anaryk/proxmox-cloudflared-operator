# pco network managed

The managed network is an SDN vnet of pco's own, pconet in the zone pco of type simple,
without a subnet: pco's leg is its only gateway, and pco gives the guests that consent a
card on it with an address of managed.subnet. pco network managed enable makes it.

## Usage

```text
pco network managed [flags]
pco network managed [command]
```

## Examples

```text
# On a host: make the zone, the vnet and the network, without a question
pco network managed enable --yes

# In an appliance, once the node ran pco appliance managed enable
pco network managed enable
```

## Commands

- [pco network managed enable](pco-network-managed-enable.md): Make the managed network

## Flags

```text
-h, --help   help for managed
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
