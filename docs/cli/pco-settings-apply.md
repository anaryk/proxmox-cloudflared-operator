# pco settings apply

Save the settings of a file: what pco settings show `--json` printed, with the settings in it
edited. Its revision is sent with them, and the daemon refuses the settings when someone
saved others since: show them again and edit those. A setting out of its range is refused
with its name. Leaving observe-only mode is no setting: that is pco apply. The settings read
only at start take effect once pco is restarted (systemctl restart pco).

With `--json` the answer of the daemon is printed as it sent it, re-indented, with control and
bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco settings apply <file> [flags]
```

## Examples

```text
# Save the settings of the edited file
pco settings apply /root/pco-settings.json

# Then restart pco if it says that a setting is read only at start
systemctl restart pco
```

## Flags

```text
-h, --help   help for apply
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco settings](pco-settings.md): Show the settings and save them
- [Command reference](index.md): every command of pco, by group
