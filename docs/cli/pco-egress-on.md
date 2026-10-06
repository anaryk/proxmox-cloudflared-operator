# pco egress on

Let the daemon and the boot unit load the egress table again, and load it, with the
resolvers of the node and no targets, unless it is loaded as it should be: the daemon adds
the verified targets at its next cycle. The exit status is 1 when the resolvers of the node
could not be read. It runs as root.

## Usage

```text
pco egress on [flags]
```

## Examples

```text
# Switch the filter back on
pco egress on

# And see what it holds after the next cycle
pco egress show
```

## Flags

```text
-h, --help   help for on
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
- [Command reference](index.md): every command of pco, by group
