# pco events

List the events the daemon keeps, oldest first: routes that changed state, conflicts,
what was applied at Cloudflare, holds that began or ended, problems that appeared and what
admins did. The daemon keeps the last thousand since it started; the journal has them all
(journalctl -u pco). `--since` takes a duration such as 10m, or a time in RFC 3339 format.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco events [--since <duration|time>] [flags]
```

## Examples

```text
# Every event the daemon keeps
pco events

# Those of the last ten minutes
pco events --since 10m

# Those after noon UTC on 1 October 2026, as JSON
pco events --since 2026-10-01T12:00:00Z --json
```

## Flags

```text
-h, --help           help for events
    --since string   list only the events after this: a duration back from now, such as 10m, or a time in RFC 3339 format
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
