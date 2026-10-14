# pco node

The nodes that run pco, as the controller's registry has them. A node joins with pco setup
(a host) or a join token (an appliance agent), is admitted once it reports, serves the shared
tunnel while it is active, is drained before maintenance and forgotten once it is gone. Every
command but list asks the controller: on another node they say which node to run them on.

## Usage

```text
pco node [command]
```

## Examples

```text
# The nodes
pco node list

# Take a node out of the shared tunnel before its maintenance
pco node drain pve2

# Remove a node that is gone
pco node forget pve3
```

## Commands

- [pco node admit](pco-node-admit.md): Admit a node that joins
- [pco node drain](pco-node-drain.md): Take a node out of the shared tunnel before its maintenance
- [pco node forget](pco-node-forget.md): Remove a node that is gone, and the secrets it held
- [pco node list](pco-node-list.md): List the nodes and their states
- [pco node set](pco-node-set.md): Change what the admin may of a node's entry
- [pco node undrain](pco-node-undrain.md): Put a drained node back among the replicas

## Flags

```text
-h, --help   help for node
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
