# pco version

Print the build version. With `--json` it is a JSON object that also names the schema
version of the store this build reads and writes, which pco upgrade `--rollback` asks the
pco it would go back to. It does not ask the daemon, and anyone may run it.

## Usage

```text
pco version [flags]
```

## Examples

```text
# The version, the commit and the date of this build
pco version

# As JSON, with the schema version of the store
pco version --json
```

## Flags

```text
-h, --help   help for version
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
