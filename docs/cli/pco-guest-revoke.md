# pco guest revoke

Remove the approval of a guest. While the admission mode is approve, its routes are no
longer published and its hostnames are held for it. The approval is shown first, and the
question needs a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco guest revoke <owner> [flags]
```

## Examples

```text
# Remove the approval of qemu/101
pco guest revoke qemu/101

# The same from a script, without the question
pco guest revoke qemu/101 --yes
```

## Flags

```text
-h, --help   help for revoke
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco guest](pco-guest.md): Approve guests for publishing, for admission mode approve
- [Command reference](index.md): every command of pco, by group
