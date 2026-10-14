# pco agent status

Show the agent of this node as it sees itself: the controller it follows and since when none
answered, the epoch and the seq of the desired state it applied and how old that is, its
connectors, the size of its egress set, whether it stood down and why, its certificate's
fingerprint and end, the ready replicas of each tunnel by the controller's last heartbeat, and
its problems. The fingerprint is what pco node admit `--fingerprint` takes for an appliance
agent. The exit status is 1 when it stood down or has a problem.

With `--json` the answer of the daemon is printed as it was sent.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco agent status [flags]
```

## Examples

```text
# The agent of this node
pco agent status

# In an appliance agent container, from the node
pct exec 9003 -- pco agent status
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

- [pco agent](pco-agent.md): The agent of this node
- [Command reference](index.md): every command of pco, by group
