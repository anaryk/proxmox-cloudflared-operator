# Remove a node

This guide takes a node out of a cluster that runs pco, while the other nodes keep the install
running: a node you retire, one that is dead, and one you replace under the same name. The
examples remove `node3` from a cluster whose controller is `node1`. A node whose disk left your
hands needs more: [A compromised node](compromised-node.md).

## When you need it

- A node is retired, or leaves the Proxmox VE cluster.
- A node is dead, or was reinstalled, and its entry stays `down` in `pco node list`.
- A node is replaced by a new machine of the same name.

## Before you start

- You are root on the controller, and on the node when it still runs.
- The guests of the node whose routes are pinned to it, the ones on its managed network or on a
  bridge only it has, are migrated first or may go down with it: `pco status --json | jq -r
  '.routes[] | select(.placement.node == "node3") | .hostname'` lists them.
- Another node is `active`, so that the shared tunnel keeps a replica.

## Steps: a node that still runs

1. Take the node out of the shared tunnel, on the controller:

   ```sh
   pco node drain node3
   ```

   Its connector of the shared tunnel stops, gracefully, and the requests go to the other
   replicas; the routes pinned to it are served on until their guests migrate. The last active
   replica is not drained without `--force`. While a guest with a pinned route still runs there,
   migrate it now: its routes follow it to its new node.

2. On `node3`, uninstall pco:

   ```sh
   pco uninstall
   ```

   It lists what goes and asks once. The node hands the lease off if it holds it, stops and
   removes its connectors, its egress filter, its gateway, its web interface and its state, and
   takes its address out of the ipset `pco-nodes`; its entry becomes `removed`, so that its
   certificate is refused from then on. What the nodes share, the store, the credentials and what
   is at Cloudflare and in Proxmox, stays. [Uninstall](../uninstall.md#one-node) has the detail.

3. On the controller, the entry is `removed`, and the controller deletes the node's pinned
   tunnels once nothing points at them:

   ```sh
   pco node list
   ```

4. Take the node out of the Proxmox VE cluster when it is leaving it, with `pvecm delnode node3`
   on another node, once it is powered off, as Proxmox describes.

Uninstalling rotates no secret: the run tokens on the node are wiped, and while the node was a
member of the cluster its root could read `/etc/pve/priv` anyway. If its disk leaves your
hands, follow [A compromised node](compromised-node.md) once it left the cluster.

## Steps: a node that is dead

A node that cannot run `pco uninstall` is forgotten from the controller.

1. Make sure it is off for good, then forget it:

   ```sh
   pco node forget node3
   ```

   ```text
   Node node3 is forgotten.
     - the entry of the node is removed: its agent is refused from its next request
     - its connections to this controller are closed
     - its pinned tunnels are deleted in the next cycle
     - the secret of tunnel pco-7f3a9c0d41b2 in account 0123456789abcdef0123456789abcdef is rotated: the run token the node holds stops working, and the remaining replicas restart with the new one
   ```

   The shared tunnel gets a new secret in every account, while `cluster.rotateOnForget` is on, so
   that the run token on the node's disk stops working; the other replicas restart with the new
   one one node at a time, and the node's connector is dropped at Cloudflare. What cannot be
   done at once, the next cycles finish; in observe-only mode they wait for `pco apply`.

2. Remove it from the Proxmox VE cluster with `pvecm delnode node3`, and run
   `pco doctor --cluster`: its check `cluster members` fails while the registry holds a node
   that is not a member, and passes once it is forgotten.

With `cluster.rotateOnForget` off the node's run token keeps working; the controller then drops
at Cloudflare, by their ids, the connectors the node reported as its own whenever a listing
shows them connected again, and counts none of them as the install's.

Should the node come back with its disk as it was, its agent stands down: the controller takes
no connection from it any more, and the agent reads its entry, `removed`, in the registry every
two seconds. It stops and disables its connectors, empties its egress filter and deletes its
run tokens, and `pco agent status` says `this node was removed from install 7f3a9c0d41b2 at
<time>; ...`. `pco uninstall` on it then
removes the rest, and `pco setup` would enrol it again.

## Steps: an appliance agent container

1. Drain the node on the controller, as above, when the agent still runs.
2. On the node of the agent container, remove it:

   ```sh
   pco appliance uninstall --vmid 121
   ```

   It removes the container, its Proxmox token and its address in the controller's ipset
   `pco-agents`, and says to forget the node.
3. On the controller, forget it:

   ```sh
   pct exec 120 -- pco node forget node3
   ```

The secret of the shared tunnel is rotated, as for a host node. The credentials, the
controller's Proxmox secret and the CA stay: an agent container never held them.

## Replace a node

A new machine under the same name gets a new install of pco and keeps the node's index, so that
the MACs of its legs stay the same:

1. `pco node forget node3` on the controller, if the old node was not uninstalled.
2. `pvecm delnode node3`, then join the new machine to the Proxmox VE cluster with `pvecm add`.
3. On the new machine, install the package and run `pco setup`, as
   [Add a node](add-a-node.md) describes; for an appliance agent, a new join token.

Its pinned tunnel is made again when routes are placed there.

## Check

On the controller:

```sh
pco node list
pco cluster status
pco doctor --cluster
```

The node is `removed`, or gone from the list once forgotten and replaced; `pco cluster status`
exits 0, the replicas of the shared tunnel are `ready` on the other nodes, and `pco doctor
--cluster` has no failure in `cluster members` or `replicas <tunnel> in account <id>`.

## Undo

A node that was removed comes back as a new node: [Add a node](add-a-node.md). A drain that was
not followed by an uninstall is ended with `pco node undrain node3`.

## Read on

- [Uninstall](../uninstall.md#a-cluster): what an uninstall on a node of a cluster removes.
- [A compromised node](compromised-node.md): forgetting with `--compromised`.
- Reference: [pco node drain](../cli/pco-node-drain.md), [pco node forget](../cli/pco-node-forget.md),
  [pco uninstall](../cli/pco-uninstall.md), [pco appliance uninstall](../cli/pco-appliance-uninstall.md).
