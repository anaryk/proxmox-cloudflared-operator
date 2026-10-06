# pco route

The routes an admin writes by hand rather than in the Notes of a guest. There is one kind
of them, the manual routes of pco route manual.

## Usage

```text
pco route [command]
```

## Examples

```text
# The manual routes
pco route manual list

# Publish status.example.com to port 9000 of 10.0.5.20, with 10.0.5.0/24 in manualCIDRs
pco route manual add status.example.com --address 10.0.5.20 --port 9000
```

## Commands

- [pco route manual](pco-route-manual.md): Manual routes: hostnames published to a guest or an address without its Notes

## Flags

```text
-h, --help   help for route
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
