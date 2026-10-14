# pco node drain

Take a node out of the shared tunnel's replicas: its connector of the shared tunnel stops, and
the routes of its own guests keep being served by its pinned tunnel until they migrate. The
last active replica is not drained without `--force`, which stops the tunnel's only connector.
A node that is not active yet keeps the intent and is drained as it is admitted. Proxmox's
maintenance mode drains a node by itself while cluster.followMaintenance is on. pco node
undrain reverses it.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node drain <node> [flags]
```

## Examples

```text
# Before maintenance of pve2
pco node drain pve2

# The last replica, although nothing serves the shared tunnel then
pco node drain pve1 --force
```

## Flags

```text
    --force   drain the last active replica too
-h, --help    help for drain
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
