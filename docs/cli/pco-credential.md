# pco credential

The Cloudflare API tokens the daemon works with, each stored as a credential with an id
and a label in `/etc/pve/priv/pco/credentials`, which only root can read. A token is checked
before it is stored, and the daemon never hands it out again. pco has no command that
replaces a token: add the new one, then remove the old credential.

## Usage

```text
pco credential [command]
```

## Examples

```text
# Add a token, which is asked for without being shown
pco credential add --label main

# The credentials and the state of each
pco credential list
```

## Commands

- [pco credential add](pco-credential-add.md): Check a Cloudflare API token and store it
- [pco credential check](pco-credential-check.md): Check what the token of a credential can do
- [pco credential list](pco-credential-list.md): List the stored credentials
- [pco credential remove](pco-credential-remove.md): Remove a credential, when nothing is left that it manages

## Flags

```text
-h, --help   help for credential
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
