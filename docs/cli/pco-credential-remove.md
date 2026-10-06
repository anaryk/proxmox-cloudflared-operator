# pco credential remove

Remove a stored credential. The daemon refuses while the token can still reach something
of this install, a DNS record of it in a zone the token sees or its tunnel in an account the
token sees, and names it: revoke the token at Cloudflare first. A token Cloudflare no longer
accepts reaches nothing, so its credential is removed, and the event says that what it
managed could not be checked.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco credential remove <id> [flags]
```

## Examples

```text
# Remove the credential a1b2c3d4 once its token is revoked
pco credential remove a1b2c3d4

# Find the id by the label first
pco credential list
```

## Flags

```text
-h, --help   help for remove
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco credential](pco-credential.md): Manage the Cloudflare tokens of the daemon
- [Command reference](index.md): every command of pco, by group
