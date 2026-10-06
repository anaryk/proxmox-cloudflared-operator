# pco sync

Ask the daemon for a reconcile cycle now, rather than after pollInterval. It returns once the
daemon has the request, not when the cycle is done; requests made while a cycle waits to
start are one cycle. pco status shows what the cycle found.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco sync [flags]
```

## Examples

```text
# Run a cycle now, after a change to the Notes of a guest
pco sync

# And see what it found once it is done
pco status
```

## Flags

```text
-h, --help   help for sync
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
