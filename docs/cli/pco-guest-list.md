# pco guest list

List the approved guests, each with the identity it was approved in and whether it still
has it, the guests whose routes wait for an approval, and the guests with the gate tag as
the last cycle listed them, with how many routes and issues each has.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped. It
holds the approvals; pco status `--json` has the guests that wait.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco guest list [flags]
```

## Examples

```text
# The approved guests, those that wait, and the tagged guests
pco guest list

# The approvals as JSON
pco guest list --json
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

- [pco guest](pco-guest.md): Approve guests for publishing, for admission mode approve
- [Command reference](index.md): every command of pco, by group
