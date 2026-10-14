# A compromised node

This guide is for a node of a cluster whose disk is out of your hands: stolen, sold, sent back
under warranty, or taken over. On a host its copy of the cluster filesystem holds every secret of
the install: the Cloudflare API tokens, the secret of the Proxmox token, the key of pco's CA and
the run tokens of the tunnels. The guide cuts the node off, rotates every one of those secrets in
an order that keeps each new one from the node, and lists what the disk holds that pco cannot
rotate. The examples forget `node3`; the controller is `node1`.

## When you need it

- The disk of a node, or a backup of it, left your hands while it held the cluster filesystem.
- You suspect that someone had root on a node.

For a node that is only dead, [Remove a node](remove-a-node.md) is enough.

## Before you start

- You are root on the controller, on a terminal.
- You can make new Cloudflare API tokens for every account and zone the install serves, as
  [Cloudflare token](../cloudflare-token.md) describes, and revoke the old ones in the dashboard.
- Read [what a compromised forget leaves open](../security.md#what-a-compromised-forget-leaves-open-and-for-how-long):
  until the last step ends, some of the old secrets still work.

## Steps

1. **Take the node out of the Proxmox VE cluster.** Everything the rotation makes goes into the
   cluster filesystem, which a member of the cluster with the authkey on its disk receives the
   moment it is online. Power it off or cut it off, then on another node:

   ```sh
   pvecm delnode node3
   ```

   Until then the forget refuses:

   ```text
   pco: run pvecm delnode node3 first: a node still in the cluster would receive every new secret
   ```

2. **Forget it as compromised**, on the controller:

   ```sh
   pco node forget node3 --compromised
   ```

   It asks once, then cuts the node off before it rotates anything, and says what follows:

   ```text
   Node node3 is forgotten.
     - the entry of the node is removed: its agent is refused from its next request
     - its connections to this controller are closed
     - its pinned tunnels are deleted in the next cycle
     - the rotation of every secret of the install began; pco cluster status follows it, and a controller that takes over finishes it

   The rotation of every secret runs step by step; pco cluster status follows it:
     - credentials
     - proxmox
     - tunnels
     - rollout
     - ca
     - previous

   What the node's disk holds that pco cannot rotate; change these by hand:
     - the corosync authkey (/etc/corosync/authkey): a node that keeps it can join the cluster again; pvecm updatecerts after a new key
     - the SSH keys of root (/root/.ssh, /etc/ssh/ssh_host_*): the other nodes trust them until they are replaced
     - the key of the cluster CA (/etc/pve/priv/pve-root-ca.key): every node's pveproxy certificate is signed with it
   ```

   Every join token not used yet is deleted too; make new ones afterwards.

3. **Replace the Cloudflare API tokens**, which only you can: for each credential, make a new
   token at Cloudflare and add it, then remove the old credential:

   ```sh
   pco credential list
   pco credential add --label main-2
   pco credential remove <old id>
   ```

   Then revoke the old tokens in the Cloudflare dashboard: pco cannot, and nothing else ends
   them. The rotation goes on only once every stored credential was added after the forget.

4. **Give the Proxmox token a new secret**, on the controller:

   ```sh
   pco setup --repair
   ```

5. **Let the daemon finish.** From here it runs by itself, in cycles that may write: not in
   observe-only mode, and not while a cycle holds. It gives every tunnel of the install, shared
   and pinned, a new secret, waits until every connector of the remaining nodes runs ready with
   the new run token, one node at a time, ends the connections made with the old secret, and
   then rotates the CA. The old CA is accepted for ten more minutes, in which every node issues
   its certificate again; after that only certificates under the new CA are. Follow it:

   ```sh
   pco cluster status
   ```

   ```text
   Rotation:    running for node node3: 3 of 6 steps done, waiting on rollout
   ```

   until it says `finished for node node3`. A controller that takes over in the middle goes on
   from the step recorded.

6. **Change by hand what pco cannot rotate**, the three items the forget listed, as the
   documentation of Proxmox VE describes for each. Decide on each: a disk that held them can pass
   for a node of the cluster to whatever trusts them.

7. **Make a new backup.** An export bundle made before the rotation holds the old CA and, with
   `--credentials`, the old tokens; it is useless against the new ones and dangerous with its
   passphrase. Make a new one with `pco export` and destroy the old.

A second node forgotten as compromised while a rotation runs joins it: every step that ran while
it was a member is done again, with new credentials and a new Proxmox secret, so steps 3 and 4
come again.

## An appliance agent container

An agent container held less: the run tokens of its tunnels and its own Proxmox token, which only
reads. First remove the container and its token on its node, `pco appliance uninstall --vmid
<vmid>`; until the token is gone `--compromised` is refused. Then, on the controller,
`pco node forget <node> --compromised` rotates the secrets of the tunnels the container ran,
whatever `cluster.rotateOnForget` says. The credentials, the controller's Proxmox secret and the
CA stay: the container never held them. A disk of the node itself, outside the container, is a
host's disk: take the node out of the cluster and treat its secrets of Proxmox as above.

## Check

```sh
pco cluster status
pco doctor --cluster
pco credential list
```

The `Rotation:` line says `finished`, with no node refused by the new CA; `pco doctor --cluster`
has no failure in `ca rotation` or `cluster members`; every credential is new. A node that was
down through the rotation issues its certificate under the new CA when its daemon starts; until
then `pco doctor` names it.

## Undo

None. A forgotten node comes back as a new node, with a clean disk:
[Add a node](add-a-node.md).

## Read on

- [Security](../security.md#what-a-compromised-part-can-do): what each compromised part can do,
  and [what pco cannot rotate](../security.md#what-pco-cannot-rotate).
- [Remove a node](remove-a-node.md): a node that is only gone.
- Reference: [pco node forget](../cli/pco-node-forget.md), [pco credential add](../cli/pco-credential-add.md),
  [pco setup](../cli/pco-setup.md), [pco cluster status](../cli/pco-cluster-status.md).
