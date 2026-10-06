# pco route manual remove

Remove a manual route: its hostname is no longer published from the next cycle, unless
another owner claims it. The route is shown first, and the question needs a terminal; a
script passes `--yes`. The route is removed at the revision shown, and not when someone
changed it since.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco route manual remove <id> [flags]
```

## Examples

```text
# Remove the manual route status
pco route manual remove status

# The same from a script, without the question
pco route manual remove status --yes
```

## Flags

```text
-h, --help   help for remove
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco route manual](pco-route-manual.md): Manual routes: hostnames published to a guest or an address without its Notes
- [Command reference](index.md): every command of pco, by group
