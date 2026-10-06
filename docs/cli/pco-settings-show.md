# pco settings show

Show the settings, the revision they are at and, for each one that has them, the range the
daemon accepts and whether it is read only when the daemon starts.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped. That is
the file pco settings apply takes: save it, edit the settings in it, and apply it.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco settings show [flags]
```

## Examples

```text
# The settings and their revision
pco settings show

# Save them to a file, to edit the settings in it
pco settings show --json > /root/pco-settings.json
```

## Flags

```text
-h, --help   help for show
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco settings](pco-settings.md): Show the settings and save them
- [Command reference](index.md): every command of pco, by group
