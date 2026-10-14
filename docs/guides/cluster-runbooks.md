# Runbooks of a cluster

These runbooks go from what you see on a cluster that runs pco to what to do about it, for the
cases that need more than a line of [Troubleshooting](../troubleshooting.md#a-cluster). Each has
the lines pco shows, what they mean, and the steps. Run the commands as root; those that change
the install on the controller, unless a step says otherwise.

## The controller is lost

**What you see.** On the nodes left, after a minute:

```text
Controller:  node1 (10.0.0.1), generation 3
Agent:       the controller is silent for 1m12s; snapshot 42 of generation 3, 1m14s old; 2 connectors, 5 egress targets
  - no controller answered for 1m12s; this node serves what it was last told
```

and a command that changes the install says
`no controller is reachable (lease held by node1 since <time>)`. The hostnames of the shared
tunnel answer through the other nodes; those pinned to `node1` do not.

**On a host**, wait. About two minutes after the last refresh of the lease another node takes
it, and about two and a half minutes after the loss it writes; `pco status` there says
`Controller:  this node (10.0.0.2), generation 4`, and the web interface's banner says the
controller moved, from where, to where and when. [Controller failover](controller-failover.md)
has the whole timeline. Step in only when no node took it after three minutes: `pco status` on
a node says why, `this node does not hold the lease yet: <why>`:

- `the cluster is not quorate`: the lost node took the quorum with it; see
  [Two nodes without a quorum device](#two-nodes-without-a-quorum-device).
- The node is not eligible: every node left is draining or marked
  `--controller-eligible=false`. On such a node, nothing else can make it eligible while no
  controller runs; bring the lost node back, or undo the marking once a controller exists.
- The builds of the nodes left speak another protocol than a registered node that is not down:
  upgrade them, or forget that node.

**In the appliance** with the controller as an HA resource, the HA manager starts it on another
node; see [Run the appliance's controller as an HA resource](appliance-ha.md). Without HA, see
the next runbook.

## The appliance's controller is lost

**What you see.** Every agent container says `no controller answered for <time>; this node serves
what it was last told`, and the controller's container is gone with its node.

The agents serve on, and nothing changes at Cloudflare. To get a controller back:

1. When its node or a backup of the container comes back, start it, or restore it with
   `pct restore` and run `pco appliance repair --vmid <vmid> --recover` on the node, as
   [Restore the appliance from a backup](appliance-restore.md) describes. The state volume was
   left out of the backup, so the install is adopted from Cloudflare, in observe-only mode.
2. A controller whose state volume came back is the one the agents know. One whose volume was
   made anew has a new CA, which the agent containers do not trust: enrol each again. On its
   node, `pco appliance uninstall --vmid <agent vmid>`; on the controller,
   `pco cluster join-token --node <node> --file /root/<node>.join`; on the node,
   `pco appliance install --kind agent --join-file /root/<node>.join`; and
   `pco node admit <node> --fingerprint <fingerprint>` on the controller.
3. Put back what the restore did not bring: the settings, approvals, acknowledged segments and
   manual routes, as [Back up and recover](backup-and-recovery.md#recover-after-a-lost-store)
   does on a host. Then `pco plan`, and `pco apply`.

## Behind after a restore

**What you see.** On the controller, `pco status`, and `pco doctor` failing `lease`:

```text
the store is older than what the agents remember (restored?); pco promote shows the differences and draws a new epoch
the agents of node2 (epoch 7.k2x9), node3 (epoch 7.k2x9) follow an epoch above this controller's; pco promote draws above it, or forget a node that should not
```

or `the store went backwards (restored?); pco promote adopts it`, or
`this store was made in cluster <name> (<fingerprint>); pco promote here makes this cluster its writer`.
The controller writes nothing anywhere; every connector serves on.

**What it means.** The store is older than what the cluster did since: `/etc/pve/pco` was
restored, or a node's disk copied into another cluster. Writing from it could put back what was
decided since, an approval revoked or a node forgotten.

1. Make sure no other controller writes with a newer store: another cluster that holds a copy of
   it, or a part of this cluster under a quorum override (see
   [Writers foreign after a quorum override](#writers-foreign-after-a-quorum-override)). If one
   does, stop pco there first.
2. On the controller, read what differs:

   ```sh
   pco promote
   ```

   It shows the epoch each agent follows, the nodes and their states, the revisions, the cluster,
   and the generation of the sentinels at Cloudflare, and asks.
3. Either fix the store, by restoring a newer copy, or confirm. A store of another cluster is
   promoted only once you type the name of the cluster it was made in, or its fingerprint. The
   promotion draws an epoch above every sentinel and every agent and starts in observe-only mode.
4. Read what the old store brings back, then publish:

   ```sh
   pco plan
   pco guest list
   pco node list
   pco apply
   ```

   A node that was forgotten after the copy was made is in the registry again: forget it again.

## Writers foreign after a quorum override

**What you see.** `pco doctor` fails `quorum override`:

```text
✗ quorum override      corosync.conf overrides the quorum: two_node; two parts of the cluster may both have quorum, and pco relies on the sentinel of its writer alone
```

and on one side or both, some tunnels say `foreign` or `stale` in `pco status`, with
`<tunnel> in account <id>: the configuration was written by generation <n> nonce <nonce>, which leader.json does not know: <hint>; writing stops`.

**What it means.** With `two_node` and no quorum device, `expected_votes` or `wait_for_all: 0` in
`/etc/pve/corosync.conf`, two parts of a cluster that cannot see each other are each quorate. Each
takes a lease of its own, draws an epoch from the same `leader.json` and writes. The sentinel at
Cloudflare is all that stands between them: at each tunnel the first to write wins, and the
writers may split the tunnels between them. Every connector keeps serving.

1. Decide which side writes: the one with the newer decisions in its store, usually the one
   with more nodes.
2. Stop pco on every node of the other side: `systemctl stop pco`. Its connectors keep running.
3. Remove the override from the quorum section of `/etc/pve/corosync.conf`, and let the cluster
   join again, as Proxmox describes. The cluster filesystem of the side that was not quorate is
   then replaced by the other's.
4. On the controller of the side that writes:

   ```sh
   pco promote
   pco plan
   pco apply
   ```

   The promotion draws above both epochs, so the other side's sentinels stop counting.
5. Start pco on the other nodes again, `systemctl start pco`. Their agents follow the higher
   epoch; what their side decided while it wrote alone is lost with its store.

## An agent refuses the controller's epoch

**What you see.** On a node, under the `Agent:` line of `pco status`:

```text
  - refused a desired state: snapshot of a lower epoch: 6.f00d, this agent follows 7.k2x9
```

and on the controller the lines of [Behind after a restore](#behind-after-a-restore).

**What it means.** The agent remembers a later epoch than the controller sends, in
`/var/lib/pco/agent/epoch.json`, and takes nothing from it. That is the check that keeps a
store that went back from writing: the controller is behind. The answer is almost always
`pco promote` on the controller, as above.

[`pco agent reset-epoch`](../cli/pco-agent-reset-epoch.md) makes one agent forget what it
remembers instead. It is right only when
all of these hold: the store was restored on purpose, every node is to follow it, you checked
that no other controller writes with a newer store, and `pco promote` is not possible, as when
the node's memory is damaged. It is dangerous because it takes the check away: an agent that
forgot its epoch follows whatever controller of the install answers, a stale copy included.

```sh
pco agent reset-epoch
systemctl restart pco
```

An agent that refuses a higher epoch does not exist: an agent always follows a higher one of its
own install, and refuses one above 2^53 as forged.

## Unpublish at once, without a controller

**When.** A published guest must stop being reachable now, and no controller answers, or you
cannot wait for a cycle.

On every node that runs a replica of its tunnel, which is every active node for a shared route,
or the node it is pinned to:

```sh
pco egress block 10.0.0.7
```

The connector of that node can no longer reach the address: the request gets a 502. The block is
local and kept in `/var/lib/pco/egress-blocked.json` until `pco egress unblock 10.0.0.7`, and a
controller that comes back honours it. Stopping the guest, `qm stop` or `pct stop`, does the same
for every node at once. The hostname and its record stay until a controller withdraws them,
which you ask for by removing the route from the guest's Notes.

## A node is dead, or its disk left your hands

A node that is dead and stays yours: `pco node forget <node>` on the controller, then
`pvecm delnode <node>`; [Remove a node](remove-a-node.md#steps-a-node-that-is-dead).

A node whose disk is out of your hands, stolen, sold or returned: its copy of the cluster
filesystem holds every secret of the install. `pvecm delnode <node>` first, then
`pco node forget <node> --compromised`, which rotates every secret pco made, and then by hand
what pco cannot rotate: the corosync authkey, the SSH keys of root and the key of the cluster CA
of Proxmox VE. [A compromised node](compromised-node.md) has the steps.

## A contested join token

**What you see.** In `pco node list` on the controller:

```text
NAME   STATE      ROLE   KIND       INDEX  ADDRESS  AGENT
node3  contested  agent  appliance  -      -        no status yet

node3 is contested (its join token was misused; pco node forget ends it); its join token was used:
  2026-10-14T13:57:00+02:00 from 10.0.0.40, container 9004, MACs bc:24:11:00:00:40
  2026-10-14T13:58:00+02:00 from 10.0.0.41, container 9005, MACs -
```

and a security event `node node3 is contested: <why>`.

**What it means.** The token of `node3` was used twice, or its first use came from a container
that is not what it said. Someone else may hold the token. Nothing is admitted.

1. Find out who used it: the times, addresses, containers and MACs of each use above.
2. Forget the node, which removes the entry and the certificate:

   ```sh
   pco node forget node3
   ```

3. Remove the container that should not have joined, if there is one, and make a new token,
   kept to root, for the right one: [Add a node](add-a-node.md#steps-an-appliance-agent).

## Two nodes without a quorum device

**What you see.** `pco doctor` warns:

```text
! quorum device        two nodes without a quorum device: when one is lost, the other has no quorum and pco on it writes nothing
```

**What it means.** A cluster of two loses its quorum when either node is down. The connectors of
the node left serve on, but nothing is written, and no controller takes over, until the other
node is back.

1. Set up a quorum device on a machine outside the cluster, as Proxmox describes:

   ```sh
   pvecm qdevice setup 10.0.0.9
   ```

2. Never use `two_node: 1` or `expected_votes` instead: they let both nodes be quorate when they
   cannot see each other, and then both write
   ([Writers foreign after a quorum override](#writers-foreign-after-a-quorum-override)).

A rolling restart of the connectors of a cluster of two takes at least a minute for each node,
as a connector is restarted only while the other node's replica is ready.

## The agent channel is down while corosync works

**What you see.** On the controller, `pco doctor` fails `agent channel`, and `pco status` says:

```text
.members lists 2 nodes online that have sent no status (node2, node3): check cluster.agentNetwork and the 8644 rule
```

`pco node list` shows those nodes `unknown`. On each of them, `pco doctor` fails `agent dial`.

**What it means.** Corosync, on its own network, sees the nodes, so the controller holds the
lease and nobody takes over; but their agents cannot reach it on TCP 8644. They serve on from
their last desired state, and the controller withdraws nothing for them, but no change reaches
them either.

1. On a node named, find what is in the way: the datacenter firewall (the ipset `pco-nodes` must
   hold the node's agent address, and the rule `IN ACCEPT -p tcp -dport 8644 -source +pco-nodes`
   be there; `pco setup` on the node puts both back), or a network `cluster.agentNetwork` names
   that the nodes do not share.
2. When only the controller's node is cut off from the agents' network, move the controller to a
   node they reach:

   ```sh
   pco cluster handoff --to node2
   ```

## HA fences the node of the controller

**What you see.** A node that runs the appliance's controller as an HA resource resets by
itself when it loses the quorum, and every guest on it with it.

**What it means.** That is how Proxmox's HA manager makes sure a resource never runs twice: the
node that runs it arms its watchdog, and fences itself when it cannot reach the quorum. For pco's
controller it is the price of an automatic failover.

1. Keep the controller on nodes where a reset is acceptable, with the rule the installer made from
   `--ha-nodes`; change it with `ha-manager rules set node-affinity pco-<vmid> --nodes <nodes>`, on
   Proxmox VE 8 with `ha-manager groupset pco-<vmid> --nodes <nodes>`.
2. Keep the quorum stable: a quorum device for two nodes, a corosync network of its own.
3. If no node may take that risk, take the controller out of HA
   ([Undo](appliance-ha.md#undo)); a lost controller is then brought back by hand.

## An upgrade is not finished

**What you see.** `pco doctor` warns `upgrade`, and `pco cluster status` says:

```text
Upgrade:     not finished since 2026-10-01T14:00:00+02:00, from 0.9.0 (pco cluster finish-upgrade); still on an older build: node3
```

and `pco settings apply` of a `cluster` setting says
`finish the upgrade first (pco cluster finish-upgrade): the previous release would refuse the settings file`.

**What it means.** The cluster runs a release that wrote what it needs beside what the earlier one
reads, so that the earlier one can still be installed again. Until the upgrade is finished, that
is the way back.

- **To go on**, upgrade the nodes still on the older build, forget the ones that are gone, make a
  backup to keep, and finish:

  ```sh
  pco export /root/pco-before-finish.bundle
  pco cluster finish-upgrade
  ```

  It is refused while a node that is not removed runs an older build, a down node included. It
  copies the store below `/var/lib/pco/backup` on this node first, but keep the bundle: it holds
  the install, and the copy holds only this node's view of it.
- **To go back**, before the upgrade is finished: the earlier release ran on one node, and
  holds every cycle while the registry names another node, a removed one too. Take the nodes
  that joined since out, as [Remove a node](remove-a-node.md) says, remove their entries,
  `/etc/pve/pco/nodes/<node>.json`, by hand, and on the node that ran it install the earlier
  package again:

  ```sh
  apt install --allow-downgrades ./pco_0.9.0_amd64.deb
  ```

  It finds the objects it wrote as they were, and serves the same routes. It reads `leader.json`
  as this release left it, with the generation drawn since, and finds that generation's
  sentinel at Cloudflare as its own.

## Read on

- [Troubleshooting](../troubleshooting.md#a-cluster): the shorter cases.
- [Problems](../problems.md#the-lease): every line of the lease and the nodes.
- [Run pco on a cluster](clusters.md), [Controller failover](controller-failover.md).
