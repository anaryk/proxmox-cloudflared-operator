# pco cluster status

Show the cluster as the controller's last cycle found it: the controller and the epoch, who
holds the lease and how long ago it was refreshed, the quorum and its device or overrides,
whether every node's forwarding-table watch covers the cluster, an upgrade not finished, the
rotation of secrets after a node was forgotten as compromised, and each node with its state,
role, kind, build and protocol, when its agent last reported, whether its watch runs and its
replica of the shared tunnel is ready, how its targets fare, the routes pinned to it, when its
certificate ends and the skew of its clock. The exit status is 1 when a node is unknown, down
or contested, or a rotation waits on the admin.

With `--json` the answer of the daemon is printed as it was sent.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco cluster status [flags]
```

## Examples

```text
# The cluster
pco cluster status

# The nodes alone, as JSON
pco cluster status --json | jq '.nodes[] | {name, state, statusAge}'
```

## Flags

```text
-h, --help   help for status
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco cluster](pco-cluster.md): The cluster: its status, the lease, join tokens, the upgrade
- [Command reference](index.md): every command of pco, by group
