# pco credential add

Check what a Cloudflare API token can do and store it when the check passes. The
token is read from the file, or from standard input; on a terminal it is asked for
without being shown. It is never taken from an argument, which others could read. A
token the check refuses is not stored, and what it can do is shown. The daemon runs a
cycle once it is stored. With `--json` the credential and its check are printed as JSON.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco credential add --label <label> [--token-file <file>] [flags]
```

## Examples

```text
# Ask for the token without showing it
pco credential add --label main

# Read it from a file only root can read
pco credential add --label main --token-file /root/cf-token

# Read it from standard input
pco credential add --label spare < /root/cf-token-spare
```

## Flags

```text
-h, --help                help for add
    --label string        name of the credential
    --token-file string   read the token from this file instead of standard input
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco credential](pco-credential.md): Manage the Cloudflare tokens of the daemon
- [Command reference](index.md): every command of pco, by group
