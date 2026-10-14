# Run the appliance's controller as an HA resource

This guide makes the controller container of a cluster of appliances a Proxmox HA resource, so
that the HA manager starts it on another node when its node is lost: what it costs the node that
runs it, the storage it needs, the rule that keeps it on the nodes you choose, and what a
relocation looks like. The examples have the controller in container 120 on `node1`, and the
nodes `node1` and `node2` it may run on.

## When you need it

You run the appliance on a cluster, and changes should go on being made when the controller's
node is lost, without your restoring the controller by hand. Without HA, the agent containers
serve on from their last desired state, and nothing is published or withdrawn until the
controller is back.

## Read this first

- **An HA resource arms the watchdog of the node that runs it.** A node with an active HA
  resource that loses the quorum fences itself: it resets, and with it every guest that runs on
  it. Choose for the controller nodes on which that is acceptable, with the rule below.
- **The storage decides what a relocation brings back.** On shared storage, Ceph or NFS with
  `shared` set, the relocated controller is the same volume and goes on as it was. On replicated
  storage (`pvesr`) the volume is as old as the last replication, which can undo recent
  decisions, a revoked approval or a forgotten node among them; the relocated controller then
  writes nothing while the agents follow a later epoch, until you recover it. On local storage
  without replication it cannot be relocated at all.
- **The controller serves its node.** A controller container runs the connectors and legs of the
  node it was installed on. After a relocation it runs them from the node it runs on now, with
  that node's bridges; `pco doctor --cluster` warns `controller ha` while it does.
- **Agent containers are never HA resources.** Each is bound to its node, and stops serving when
  it is made one.

## Before you start

- A cluster with a quorum when a node is lost: three nodes, or two with a quorum device.
- A storage for the controller that every node of the rule reaches, shared for automatic
  failover.
- The HA shutdown policy of the cluster set to `migrate`, so that a clean shutdown or reboot of
  the controller's node moves the controller first instead of stopping it:

  ```sh
  pvesh set /cluster/options --ha shutdown_policy=migrate
  ```

## Steps

1. **A new controller**, on a node of the rule:

   ```sh
   pco appliance install --kind controller --storage ceph-ct --ha --ha-nodes node1,node2 --checksums checksums.txt
   ```

   The installer says what HA does to the node before it asks, records the storage and whether it
   is shared or replicated, makes the container an HA resource that is started, and keeps it on
   `--ha-nodes` with the rule `pco-120`: a node-affinity rule on Proxmox VE 9, a restricted group
   on 8.

2. **A combined appliance that becomes the controller** gets HA with its conversion:

   ```sh
   pco appliance convert --vmid 120 --ha --ha-nodes node1,node2
   ```

3. **A controller installed without `--ha`** is made one by hand, as for any container:
   `ha-manager add ct:120 --state started`, and a rule or group that keeps it on the nodes you
   choose. `pco doctor --cluster` checks it the same way.

4. Add the agent containers of the other nodes, as
   [Install the appliance](appliance-install.md#on-a-cluster) describes.

## What a relocation looks like

When the controller's node is lost, the HA manager waits until that node has fenced itself, then
starts the container on another node of the rule. That takes a few minutes. Meanwhile the agent
containers serve on.

- **On shared storage** the controller starts on the same volume, draws its next epoch, and goes
  on writing; the agents follow the higher epoch. `pco status` in it shows the new generation.
- **On replicated storage** the controller writes nothing while more than half of the agents of
  the other nodes with a fresh report, and at least two, follow a later epoch than its own:

  ```text
  the agent of node node2 follows epoch 7.k2x9, above this controller's 6.f00d: the store is older than what the agents were last told, as after a restore or a relocation on replicated storage; nothing is written until pco appliance recover draws above it
  ```

  Read what may have been undone since the last replication: approvals, settings, forgotten
  nodes. Put right what you must, then draw an epoch above the agents in the container:

  ```sh
  pct exec 120 -- pco appliance recover
  ```

  It starts in observe-only mode: read `pco plan`, then `pco apply`. With a single agent
  container the controller cannot tell that its volume is old: one agent alone, broken or taken
  over, holds nobody, so on replicated storage keep at least two agent containers, or prefer
  shared storage.

## Check

```sh
pct exec 120 -- pco doctor --cluster
ha-manager status
```

`controller ha` is fine, or warns only for what you chose: replicated storage, or the controller
running away from the node it serves. `ha-manager status` lists `ct:120` as `started`.

## Undo

Take the controller out of HA, and its rule:

```sh
ha-manager remove ct:120
ha-manager rules remove pco-120
```

On Proxmox VE 8 the group goes with `ha-manager groupremove pco-120`. `pco appliance uninstall`
removes the rule or group of the controller it removes.

## Read on

- [Appliance](../appliance.md#the-controller-and-proxmox-ha): the controller and Proxmox HA.
- [Controller failover](controller-failover.md#what-goes-on-in-the-appliance): what each storage
  gives.
- [The runbooks of a cluster](cluster-runbooks.md#ha-fences-the-node-of-the-controller): the
  fencing of the controller's node.
- Reference: [pco appliance install](../cli/pco-appliance-install.md),
  [pco appliance convert](../cli/pco-appliance-convert.md),
  [pco appliance recover](../cli/pco-appliance-recover.md).
