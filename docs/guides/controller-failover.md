# Controller failover

This guide says what happens when the controller of a cluster goes away, and what you see while
another one takes over: on a host, by the lease; in the appliance, by Proxmox's high availability.
It is mostly a guide of waiting: the takeover runs by itself, and the steps are what to look at,
and when to step in. The examples have `node1` as the controller that is lost, and `node2` and
`node3`.

## When you need it

- The controller's node lost its power, crashed or was cut off, and you want to know what goes
  on.
- You plan maintenance of the controller's node and want the hand-over to take seconds.
- You test failover before you rely on it.

## Before you start

- The cluster has a quorum without the controller's node: three nodes or more, or two with a
  quorum device. A cluster of two without one has no controller while either node is down, and
  serves on unchanged until the node is back.
- The clocks of the nodes are in step, and at least one other node may be the controller:
  `pco node list` on the controller, with no `--controller-eligible=false` on all of them.

## What goes on, on a host

| Time after the loss | What happens | What you see |
|---|---|---|
| 0 | `node1` is lost. Every connector of `node2` and `node3` serves on, from the last desired state each was given. Cloudflare sends the requests of the shared tunnel to the replicas left once it notices the lost connector gone. | Hostnames of the shared tunnel answer; those pinned to `node1` get Cloudflare's error 1033, as their tunnel's only connector and their guests were there. |
| 0 to 30 s | The agents get no heartbeat. `.members` lists `node1` offline at once. | `pco status` on `node2`: the heartbeat of its `Agent:` line grows old, and after a minute it says `the controller is silent for 1m0s`. |
| 30 s | The other nodes may take the lease, and ask the cluster filesystem to remove the lock every 5 seconds; it refuses while the lock is younger than 120 seconds. | `this node does not hold the lease yet: <why>; nothing is written`. |
| about 125 s | The lock expired; one node takes it, `node2`, draws a new epoch and writes it into `leader.json`. | Event `lease`, `acquired at generation 4`; `pco status` on `node3`: `Controller:  node2 (10.0.0.2), generation 4`. |
| a few seconds later | The agent of `node3` reads `leader.json` and reports to `node2`. `node2` waits for every node that is not down to report, a minute at most. | `waiting for the agents of node3 to report to this controller; nothing is written at Cloudflare for up to <time>`. |
| about 150 s | The first cycle that writes. Placements and moves under way are in the store, so nothing moves only because the controller did. | The problems clear; `pco cluster status` on `node2`. |

In tests the lock changed hands 123 to 127 seconds after the last refresh of a node that lost its
power. The routes pinned to `node1` come back once their guests run elsewhere, as Proxmox's high
availability restarts them, and the new controller moves them to their new node's pinned tunnel.

A clean shutdown or reboot of the controller's node, and a stop of `pco.service` there, give the
lease back: another node takes it within seconds, in tests within four. A crash of the daemon
alone costs nothing: systemd starts it again, and it finds the lease still its own and keeps its
epoch.

## Steps

1. **Before maintenance of the controller's node**, hand the lease over, so that the takeover
   takes seconds:

   ```sh
   pco cluster handoff --to node2
   ```

   The controller stops writing after its running cycle and gives the lease back; for the next
   30 seconds only `node2` may take it. Then shut the node down as you would.

2. **After a loss**, wait about two and a half minutes, and follow it on any node that is left:

   ```sh
   pco status
   pco events --kind security --since 10m
   ```

   The security events have the takeover, `acquired at generation 4` on `node2`, also when the
   node that held the lease is gone.

3. **Check the new controller**, on `node2`:

   ```sh
   pco cluster status
   ```

   `node1` is `down`, the other nodes `active` and `heard ... ago`, and the replicas of the shared
   tunnel `ready` on them.

4. **Step in** when no node takes the lease after three minutes. `pco status` on a node says
   why: no quorum (`pvecm status`), no node left that may be the controller, or builds of another
   protocol. [The runbooks](cluster-runbooks.md#the-controller-is-lost) have each case.

5. **When `node1` comes back**, it passes through `joining`, and is `active` again once its agent
   reported; it does not take the lease back. Hand it back with `pco cluster handoff --to node1` if
   you want it there.

## What goes on in the appliance

The appliance has no lease. Its controller is one container, and what follows its loss depends on
whether it is a Proxmox HA resource
([Run the appliance's controller as an HA resource](appliance-ha.md)):

- **An HA resource on shared storage.** The HA manager fences the lost node, then starts the
  container on another node of its rule, on the same volume. The controller draws its next epoch
  at its start and writes on; the agents follow the higher epoch. That takes as long as Proxmox's
  HA recovery, a few minutes. A controller container serves the node it runs on, so the
  connectors of the node it was installed on run where it runs now, and `pco doctor --cluster`
  says so in `controller ha`.
- **An HA resource on replicated storage.** The volume may be one replication interval old. The
  relocated controller writes nothing while more than half of the agents of the other nodes, and
  at least two, follow a higher epoch than its own, and says so; read what changed since, then
  `pco appliance recover` in it draws an epoch above them.
- **No HA.** Nothing takes over. The agent containers serve on from their last desired state until
  the controller's node is back. When it is lost for good,
  [The appliance's controller is lost](cluster-runbooks.md#the-appliances-controller-is-lost) has
  the way back.

## Check

```sh
pco cluster status
pco doctor --cluster
```

On the new controller: no node but the lost one is `unknown` or `down`, the lease is `valid`, and
`pco doctor --cluster` has no failure.

## Undo

Hand the lease to the node you want: `pco cluster handoff --to <node>`.

## Read on

- [Architecture](../architecture.md#a-takeover): the lease and a takeover.
- [Operations](../operations.md#the-lease): the timings in the reference.
- [The runbooks of a cluster](cluster-runbooks.md): when the takeover does not go as here.
- Reference: [pco cluster handoff](../cli/pco-cluster-handoff.md), [pco status](../cli/pco-status.md).
