# pco doctor

Check what pco needs: the mode, the last cycle, the inventory, the credentials,
cloudflared, the tunnels and their connectors, the way out to Cloudflare, the writer, the
records in the way, Proxmox, the store and the lock of the node, what waits for a
confirmation and the guests that wait for approval. In the appliance it also checks what
the appliance depends on: its container and its volume, its identity, the token and the
access control around it, the way out and the egress filter as the user of the connectors,
DNS and the clock, the held packages and the versions, and the disk, the journal and the
memory. Each finding comes with what to do about it, and the exit status is 1 when a check
fails.

When the daemon is not running, root still gets the checks that need no daemon: the units
of pco, the store, cloudflared and the egress table. The rest is said not to have been made,
and the exit status is 1 when one of these fails, as it is when pco.service does not run.
It changes nothing.

On a node of a cluster it also checks the agent of the node: whether it serves, its
certificate, and on a node that is not the controller whether it hears the controller and
reaches its agent endpoint on port 8644. The controller adds the checks of the cluster: the
lease, quorum and its overrides, the members, the agent channel, the clocks, the versions, the
certificates and the CA, coverage, the replicas of each shared tunnel, the pinned tunnels, an
unfinished upgrade or rotation, and whether it reaches Cloudflare. With `--cluster` the
controller also asks the agent of every other active node for its checks and waits 30
seconds for them; their findings carry the name of the node, and an agent that did not answer
is listed. On a node that is not the controller it says where to run it.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface. The exit status is 2 when the daemon could not be asked, unless root got the
checks that need no daemon instead.

## Usage

```text
pco doctor [--cluster] [flags]
```

## Examples

```text
# Check the installation
pco doctor

# The findings as JSON, for a script or a monitor
pco doctor --json

# On the controller of a cluster: every node's checks too
pco doctor --cluster
```

## Flags

```text
    --cluster   on the controller, also ask the agents of the other nodes for their checks
-h, --help      help for doctor
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
