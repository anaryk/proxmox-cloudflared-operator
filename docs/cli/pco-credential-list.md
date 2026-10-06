# pco credential list

List the stored credentials: the id, the label, the kind and the state of each, usable,
problem, or unknown while no check has answered, with a note that says what its last
check found wrong, which zones it leaves out, and when its token has expired or expires
within 30 days.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco credential list [flags]
```

## Examples

```text
# The credentials and the state of each
pco credential list

# As JSON, for a script
pco credential list --json
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

- [pco credential](pco-credential.md): Manage the Cloudflare tokens of the daemon
- [Command reference](index.md): every command of pco, by group
