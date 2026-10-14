# pco cluster finish-upgrade

End an upgrade from an earlier release of pco. Until then the previous release can be
installed again on any node and serves the same routes, and the settings this release adds
cannot be set. It is refused while a registered node that was not forgotten runs a build that
does not read this release's store; a node that is down counts, so a dead node is forgotten
first with pco node forget. The store is copied below `/var/lib/pco/backup` on the controller's
node before anything changes; pco export is the backup to keep. The question needs a
terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco cluster finish-upgrade [flags]
```

## Examples

```text
# Once every node runs this release
pco cluster finish-upgrade

# From a script
pco cluster finish-upgrade --yes
```

## Flags

```text
-h, --help   help for finish-upgrade
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco cluster](pco-cluster.md): The cluster: its status, the lease, join tokens, the upgrade
- [Command reference](index.md): every command of pco, by group
