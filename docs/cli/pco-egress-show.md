# pco egress show

Show whether the egress filter is on and its table loaded as pco loads it, the targets and
the resolvers its sets hold, how many packets it rejected since the table was last loaded
in full, to the addresses of this node and to anything else, and the block list of this
node. It changes nothing. The exit status is 1 when the filter is off, its table is not
loaded or not as pco loads it, or the connector user does not exist. It runs as root.

## Usage

```text
pco egress show [flags]
```

## Examples

```text
# Whether the filter is on, and what it holds
pco egress show

# From a script: is the filter on and as pco loads it?
pco egress show > /dev/null && echo confined
```

## Flags

```text
-h, --help   help for show
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
- [Command reference](index.md): every command of pco, by group
