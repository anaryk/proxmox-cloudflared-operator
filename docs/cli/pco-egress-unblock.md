# pco egress unblock

Take an address off the block list of this node, and out of the blocked set of the egress
table. The daemon puts it back into the table at its next cycle if it is still a verified
target. It runs as root.

## Usage

```text
pco egress unblock <address> [flags]
```

## Examples

```text
# Let the connectors reach 10.0.0.12 again, if the daemon verifies it
pco egress unblock 10.0.0.12

# The block list that is left
pco egress show
```

## Flags

```text
-h, --help   help for unblock
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
- [Command reference](index.md): every command of pco, by group
