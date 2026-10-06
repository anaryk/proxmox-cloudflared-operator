# pco plan

Show the actions of the last cycle that were not applied and why they are held,
what waits for a confirmation, the records of someone else that stand in the way of
a hostname, and the names that point at the tunnel but lost the marker of this install.
It changes nothing.

With `--json` the whole state of the daemon is printed as the daemon sent it, re-indented,
with control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco plan [flags]
```

## Examples

```text
# What the daemon would change, and what holds it back
pco plan

# The whole state as JSON, for a script
pco plan --json
```

## Flags

```text
-h, --help   help for plan
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
