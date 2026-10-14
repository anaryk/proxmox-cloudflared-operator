# pco node list

List the nodes of the registry as the controller's last cycle found them: the state of each,
its role, kind and index, its agent address, when its agent last reported and whether its
forwarding-table watch runs. Active nodes come first, then draining, joining, unknown, down,
contested and removed ones. A node alone lists nothing. The exit status is 1 when a node is
unknown, down or contested.

With `--json` the nodes are printed as the daemon sent them.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node list [flags]
```

## Examples

```text
# The nodes
pco node list

# The nodes that are not active
pco node list --json | jq '.[] | select(.state != "active") | .name'
```

## Flags

```text
-h, --help   help for list
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
