# pco status

Show the mode of the daemon, whether the inventory is complete, the egress filter, the
routes by state, the tunnels with their connectors, the credentials, the issues found in
guest notes and the problems. The exit status is 1 when there are problems or the egress
filter does not confine the connectors.

With `--json` the state of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco status [flags]
```

## Examples

```text
# What the daemon found and did
pco status

# The problems alone, from the state as JSON
pco status --json | jq -r '.problems[]'
```

## Flags

```text
-h, --help   help for status
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
