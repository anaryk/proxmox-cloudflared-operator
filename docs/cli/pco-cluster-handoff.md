# pco cluster handoff

Hand the lease off: the controller stops writing after its running cycle and releases the
lease, and the node named with `--to` alone may take it for the next 30 seconds; without `--to`,
whichever node comes first takes it, and this node holds back as long. Use it before
maintenance of the controller's node, so that another node takes over within seconds rather
than when the lease runs out. The question needs a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco cluster handoff [flags]
```

## Examples

```text
# To whichever node comes first
pco cluster handoff

# To pve2, from a script
pco cluster handoff --to pve2 --yes
```

## Flags

```text
-h, --help        help for handoff
    --to string   the node that takes the lease; none for whichever comes first
-y, --yes         do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco cluster](pco-cluster.md): The cluster: its status, the lease, join tokens, the upgrade
- [Command reference](index.md): every command of pco, by group
