# pco claims resolve

Move the claim on a hostname from its holder to another owner that claims it: a guest,
as qemu/102, or a manual route, as `manual/<id>`. The public hostname goes to that owner,
and the holder waits for it as every other claimant does. Who holds it and who would are
shown first, and the question needs a terminal; a script passes `--yes`. The daemon refuses
an owner that does not claim the hostname.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco claims resolve <hostname> <owner> [flags]
```

## Examples

```text
# Hand www.example.com to qemu/102, which claims it too
pco claims resolve www.example.com qemu/102

# Hand api.example.com to the manual route api, from a script
pco claims resolve api.example.com manual/api --yes
```

## Flags

```text
-h, --help   help for resolve
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco claims](pco-claims.md): Show who holds each public hostname, and hand one to another owner
- [Command reference](index.md): every command of pco, by group
