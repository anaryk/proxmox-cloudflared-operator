# pco agent

The agent that runs on this node: it follows the controller where the lease is, applies the
desired state it is given, runs the connectors and the egress filter, and reports what it
sees. Both commands are answered by this node, whatever its role.

## Usage

```text
pco agent [command]
```

## Examples

```text
# What the agent of this node follows and runs
pco agent status

# Forget the epoch it remembers, after the store was restored on purpose
pco agent reset-epoch
```

## Commands

- [pco agent reset-epoch](pco-agent-reset-epoch.md): Forget the epoch the agent of this node remembers
- [pco agent status](pco-agent-status.md): Show what the agent of this node follows and runs

## Flags

```text
-h, --help   help for agent
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
