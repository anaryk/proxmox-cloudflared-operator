# pco node set

Change a node's entry in the registry: `--controller-eligible` says whether the node may hold
the lease and be the controller. A node that may not keeps serving its guests and the shared
tunnel; it only never takes over.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node set <node> [flags]
```

## Examples

```text
# Keep the lease off a small node
pco node set pve3 --controller-eligible=false

# Let it hold the lease again
pco node set pve3 --controller-eligible=true
```

## Flags

```text
    --controller-eligible string   whether the node may hold the lease: true or false
-h, --help                         help for set
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
