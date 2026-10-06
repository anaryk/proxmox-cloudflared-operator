# pco routes

List the routes sorted by hostname. The level is how strongly the address of the target
was proven to be the guest's: port, observed, or manual for a route to an address. The note
is the reason a route is not served, or its first warning.

With `--json` the whole state of the daemon is printed as the daemon sent it, re-indented,
with control and bidirectional characters escaped, and `--state` cannot be used.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco routes [flags]
```

## Examples

```text
# Every route, sorted by hostname
pco routes

# Only the routes that are held
pco routes --state held
```

## Flags

```text
-h, --help           help for routes
    --state string   show only the routes in this state: active, unreachable, withdrawn, conflict, no-zone, held, rejected, frozen
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
