# pco claims list

List who holds each public hostname, since when, and who waits for it. As the last cycle
that settled the claims found it, a claim is serving when its holder publishes the
hostname, conflict when others want it too, held when nobody serves it, and pending when
another owner served it, as after a resolve. It is unknown until a cycle of the daemon
has settled the claims.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco claims list [flags]
```

## Examples

```text
# Who holds each hostname, and who waits for it
pco claims list

# As JSON, for a script
pco claims list --json
```

## Flags

```text
-h, --help   help for list
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco claims](pco-claims.md): Show who holds each public hostname, and hand one to another owner
- [Command reference](index.md): every command of pco, by group
