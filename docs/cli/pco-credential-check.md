# pco credential check

Check what the token of a stored credential can do. A deep check proves the write
permissions by creating and deleting a test DNS record and a test tunnel; it asks
first, which needs a terminal, and a script passes `--yes`. pco credential list shows the ids.
With `--json` the credential and its check are printed as JSON.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco credential check <id> [flags]
```

## Examples

```text
# What the token of credential a1b2c3d4 can read
pco credential check a1b2c3d4

# Prove that it can write, with a test record and a test tunnel
pco credential check a1b2c3d4 --deep
```

## Flags

```text
    --deep   prove the write permissions with test objects
-h, --help   help for check
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco credential](pco-credential.md): Manage the Cloudflare tokens of the daemon
- [Command reference](index.md): every command of pco, by group
