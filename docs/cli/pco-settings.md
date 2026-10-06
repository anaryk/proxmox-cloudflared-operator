# pco settings

The settings of the install, such as pollInterval, admission and manualCIDRs, kept in
`/etc/pve/pco/meta/settings.json` with a revision that every save raises. The daemon reads
them in every cycle, and a few only when it starts.

## Usage

```text
pco settings [command]
```

## Examples

```text
# The settings and their revision
pco settings show

# Save them to a file, to edit the settings in it
pco settings show --json > /root/pco-settings.json

# Save the settings of the edited file
pco settings apply /root/pco-settings.json
```

## Commands

- [pco settings apply](pco-settings-apply.md): Save the settings of a file, at the revision they were read at
- [pco settings show](pco-settings-show.md): Show the settings with their revision

## Flags

```text
-h, --help   help for settings
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
