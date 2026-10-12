# pco managed

The managed network is an SDN vnet of pco's own, pconet, on which pco gives guests a card
with an address of managed.subnet, so that a guest on a network the node has no address in
is reached all the same. A guest gets a card when it consents: with the tag
cf-tunnel-managed, which only an admin may set while it is registered as admin-only, or
with an approval given here, in the identity the guest has then; with managed.consent at
auto also every guest with the gate tag that needs the network. The card goes after the
grace managed.detachGrace once the consent ends, and cf-tunnel-managed=off in the Notes of
a guest refuses it. pco network managed enable makes the network; pco guest approve is
another thing, the admission of a guest's routes.

## Usage

```text
pco managed [command]
```

## Examples

```text
# The cards, the guests that wait and why, and the foreign cards
pco managed status

# Consent to a card for lxc/120 in the identity it has now
pco managed approve lxc/120

# Take the consent back; the card goes after the grace
pco managed revoke lxc/120
```

## Commands

- [pco managed approve](pco-managed-approve.md): Consent to a card on the managed network for a guest
- [pco managed revoke](pco-managed-revoke.md): Take back the consent of a guest; its card goes after the grace
- [pco managed status](pco-managed-status.md): Show the managed network: its cards, the guests that wait and the foreign cards

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

- [Command reference](index.md): every command of pco, by group
