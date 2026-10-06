# pco guest

With the setting admission at approve, the routes of a tagged guest are published only once
an admin approved the guest, in the identity it has then: for a virtual machine its SMBIOS
UUID, or else its creation time, or else a hash of the MAC of its first card; for a
container a hash of the MAC of its first card. A clone, or a guest made again with a new
identity, needs an approval of its own. In either mode an approval also releases the routes
of the guest that wait at the observed level, or on a soft-denied address. The approvals
are kept in `/etc/pve/pco/approvals`.

## Usage

```text
pco guest [command]
```

## Examples

```text
# The approved guests, and those that wait
pco guest list

# Approve qemu/101 in the identity it has now
pco guest approve qemu/101
```

## Commands

- [pco guest approve](pco-guest-approve.md): Approve a guest in the identity it has now
- [pco guest list](pco-guest-list.md): List the approved guests, the guests that wait for approval and the tagged guests
- [pco guest revoke](pco-guest-revoke.md): Remove the approval of a guest

## Flags

```text
-h, --help   help for guest
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
