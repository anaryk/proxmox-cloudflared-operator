# pco egress block

Add an address to the block list of this node and take it out of the egress table at once.
The list is subtracted from every set the daemon loads, until pco egress unblock. It is
kept in `/var/lib/pco/egress-blocked.json`, for this node only. It runs as root.

## Usage

```text
pco egress block <address> [flags]
```

## Examples

```text
# Keep the connectors from 10.0.0.12
pco egress block 10.0.0.12

# And see that the table rejects it
pco egress show
```

## Flags

```text
-h, --help   help for block
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
- [Command reference](index.md): every command of pco, by group
