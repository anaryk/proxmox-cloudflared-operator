# pco route manual list

List the manual routes by id, each with the revision it is at. A manual route owns its
hostname as `manual/<id>` and competes in the claims like any guest.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco route manual list [flags]
```

## Examples

```text
# The manual routes
pco route manual list

# As JSON, for a script
pco route manual list --json
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

- [pco route manual](pco-route-manual.md): Manual routes: hostnames published to a guest or an address without its Notes
- [Command reference](index.md): every command of pco, by group
