# pco diagnose

Walk what a request for a hostname goes through: its route, its zone, its DNS record, the
rule of the tunnel, the connector, the identity and the port of its target, and last a
request the daemon makes to that target as the tunnel would. A step that fails skips the
steps after it and makes the exit status 1. It changes nothing.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco diagnose <hostname> [flags]
```

## Examples

```text
# Why app.example.com does not answer as it should
pco diagnose app.example.com

# The steps as JSON, for a script
pco diagnose app.example.com --json
```

## Flags

```text
-h, --help   help for diagnose
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
