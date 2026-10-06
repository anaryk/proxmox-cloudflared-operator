# pco route manual

A manual route publishes a hostname to a guest, whose address is proven as for a route in
its Notes, or to an IPv4 address inside the manualCIDRs of the settings, which is not
proven: its level is manual. It owns its hostname as `manual/<id>` and competes in the claims
like any guest. The routes are kept in `/etc/pve/pco/routes`, and the daemon reads them in
every cycle.

## Usage

```text
pco route manual [command]
```

## Examples

```text
# The manual routes
pco route manual list

# Publish status.example.com to port 9000 of 10.0.5.20, with 10.0.5.0/24 in manualCIDRs
pco route manual add status.example.com --address 10.0.5.20 --port 9000

# Remove the route status
pco route manual remove status
```

## Commands

- [pco route manual add](pco-route-manual-add.md): Make a manual route
- [pco route manual list](pco-route-manual-list.md): List the manual routes
- [pco route manual remove](pco-route-manual-remove.md): Remove a manual route

## Flags

```text
-h, --help   help for manual
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco route](pco-route.md): Make and remove the routes an admin writes by hand
- [Command reference](index.md): every command of pco, by group
