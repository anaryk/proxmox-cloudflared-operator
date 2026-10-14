# pco cluster

The cluster pco runs on: which node is the controller and holds the lease, the nodes and
how each fares, the quorum, the coverage of the forwarding-table watches and the rotation of
secrets after a node was forgotten as compromised. The commands that change something ask the
controller: on another node they say which node to run them on. A node alone is its own
cluster of one, and pco cluster status shows that.

## Usage

```text
pco cluster [command]
```

## Examples

```text
# The cluster as the controller found it
pco cluster status

# Hand the lease to another node before maintenance of this one
pco cluster handoff --to pve2

# A token for an appliance agent to join as node pve3
pco cluster join-token --node pve3 --file /root/pve3.join
```

## Commands

- [pco cluster finish-upgrade](pco-cluster-finish-upgrade.md): End an upgrade from an earlier release
- [pco cluster handoff](pco-cluster-handoff.md): Hand the lease to another node
- [pco cluster join-token](pco-cluster-join-token.md): Make the token an appliance agent joins with
- [pco cluster status](pco-cluster-status.md): Show the controller, the lease, the quorum and every node
- [pco cluster unblock](pco-cluster-unblock.md): End the block of an address every node blocks

## Flags

```text
-h, --help   help for cluster
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
