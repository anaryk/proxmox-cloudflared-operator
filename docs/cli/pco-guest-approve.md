# pco guest approve

Approve a guest, named as qemu/101 or lxc/200, in the identity the daemon sees it in now:
a guest re-created under the same VMID, or a clone, needs an approval of its own. A guest
that waits for approval is shown first, with the hostnames it would publish, why it waits
and what the approval records: the MACs its addresses answer from at the observed level,
and the soft-denied addresses, as the gateway of a node, it may be published at whatever
the level. The daemon refuses the approval when the guest changed since it was shown, and
a guest the last cycle did not see. `--allow-address` allows an address the guest was not
shown at. An approval admits a guest while the admission mode is approve, and releases
what waits at observed, or on a soft-denied address, in either mode.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco guest approve <owner> [flags]
```

## Examples

```text
# Approve qemu/101 in the identity it has now
pco guest approve qemu/101

# And let lxc/120 be published at the soft-denied address 192.0.2.1 too
pco guest approve lxc/120 --allow-address 192.0.2.1
```

## Flags

```text
    --allow-address stringArray   allow the guest to be published at this soft-denied address too (repeatable)
-h, --help                        help for approve
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco guest](pco-guest.md): Approve guests for publishing, for admission mode approve
- [Command reference](index.md): every command of pco, by group
