# pco egress off

Remove the egress table and keep the daemon and the boot unit from loading it again,
until pco egress on. The connectors are not confined meanwhile; pco status warns of it on
every run. The switch is kept in `/var/lib/pco/egress-off.json`, for this node only. It runs
as root.

## Usage

```text
pco egress off [flags]
```

## Examples

```text
# Switch the filter off while you look for the fault
pco egress off

# And on again once it is found
pco egress on
```

## Flags

```text
-h, --help   help for off
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
- [Command reference](index.md): every command of pco, by group
