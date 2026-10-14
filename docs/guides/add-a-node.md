# Add a node

This guide adds a node to a cluster that runs pco: a node of the Proxmox VE cluster that does not
run pco yet, or a new node you just joined to it. The node gets its certificate, joins the
install as `joining`, and becomes `active` once the controller admitted it. The examples add
`node3` to a cluster whose controller is `node1`.

## When you need it

- A node of the cluster runs guests you publish, or should run a replica of the shared tunnel,
  and has no pco.
- A new node joined the Proxmox VE cluster.
- A node was reinstalled and keeps its name; forget the old one first, as
  [Remove a node](remove-a-node.md#replace-a-node) says.

## Before you start

- The node is a member of the Proxmox VE cluster, and the cluster is quorate: `pvecm status`.
  A node that runs guests cannot join a cluster with `pvecm add`; that is Proxmox's rule.
- Its clock is in step with the others (`chronyc tracking`), as for every node of a cluster.
- It has an address of its own on the bridges and VLANs of the guests it should serve, and
  reaches Cloudflare on ports 443 and 7844.
- Its agent address, the address its name resolves to unless `cluster.agentNetwork` names
  another network, is reachable from the other nodes, and they from it, on TCP 8644.
- The package of the same release as the other nodes; `pco version` on the controller says which.

## Steps: a host node

1. Install the package on `node3`, without the setup, as [Quickstart](../quickstart.md#install)
   describes with `PCO_SKIP_SETUP=1`, or with `apt install ./pco_<version>_<arch>.deb`.

2. Run the setup on `node3`:

   ```sh
   pco setup
   ```

   It makes what the node needs in Proxmox as on the first node, installs `cloudflared` when it
   is missing, and joins the install it finds on the cluster filesystem:

   ```text
   store: install 7f3a9c0d41b2 kept; joining the nodes node1, node2
   certificates: issued the client certificate of node node3 (serial 7a20..., SHA-256 c4d1...) and the server certificate node-node3.pco-7f3a9c0d41b2.invalid, both until 2027-01-12
   node registry: registered node3 as joining; the controller admits it
   credentials: 1 stored, nothing needed
   firewall: ipset pco-nodes holds 10.0.0.3 (node3)
   firewall: the datacenter rule IN ACCEPT -p tcp -dport 8644 -source +pco-nodes is there, nothing needed
   pco.service: enabled and running
   ```

   The node issues its certificate itself, from the CA of the install on the cluster
   filesystem: root of a node is root of the cluster, so there is nothing for an admin to
   confirm. Nothing changes at Cloudflare.

3. Follow its admission on the controller, `node1`:

   ```sh
   pco node list
   ```

   ```text
   NAME   STATE    ROLE        KIND  INDEX  ADDRESS   AGENT
   node1  active   controller  host  0      10.0.0.1  heard 1s ago, watching
   node2  active   agent       host  1      10.0.0.2  heard 3s ago, watching
   node3  joining  agent       host  2      10.0.0.3  heard 2s ago, watching
   ```

   The controller sends the joining node every route of the shared tunnel to verify, and no run
   token. Once its agent has reported all of them, the routes it fails while the other replicas
   reach them move to tunnels pinned to their guests' nodes, and the node becomes `active` and
   starts its replica of the shared tunnel. The event `node node3 is active, was joining` says
   so.

4. When it stays joining, `pco status` on the controller says why:

   ```text
   node node3 stays joining: node3 fails 14 shared routes that other replicas reach, more than cluster.admissionMaxMoves allows (10); it stays joining until pco node admit
   ```

   Find why it does not reach them, usually a missing address on a bridge or VLAN, and fix that:
   it is admitted by itself once it fails few enough. To admit it as it is, which moves every
   route it fails to a pinned tunnel first:

   ```sh
   pco node admit node3
   ```

## Steps: an appliance agent

An agent container joins the controller container with a token that is good once, for one
node, for 15 minutes, and an admin admits it by the fingerprint of its certificate.

1. On the controller's node, make the token for `node3` into a file only root reads:

   ```sh
   pct exec 120 -- pco cluster join-token --node node3 --file /root/node3.join
   pct pull 120 /root/node3.join /root/node3.join
   ```

   Copy `/root/node3.join` to `node3`, with `scp` as root, and remove it from where it is not
   needed.

2. On `node3`, install the agent container with it:

   ```sh
   pco appliance install --kind agent --storage local-zfs --join-file /root/node3.join --checksums checksums.txt
   ```

   The installer pushes the token onto the agent's state volume, never into an argument or the
   environment, and the agent removes it once the controller answered. The agent joins: the
   controller checks the token and that the container is in pool `pco` on `node3`, with the MACs
   and the address it said, and records it as `joining`.

3. Read the fingerprint of the agent's certificate in the container:

   ```sh
   pct exec 121 -- pco agent status
   ```

   ```text
   Certificate: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08, ends 2027-01-12T10:00:00+02:00
   ```

4. Admit it on the controller with at least the first eight hex digits of that fingerprint:

   ```sh
   pct exec 120 -- pco node admit node3 --fingerprint 9f86d081
   ```

   `pco node list` on the controller shows every use of the join token before you do. From here
   the node goes through the same routing admission as a host node.

A token used a second time, or by a container that does not match what it said, holds the node
`contested`: nothing is admitted until `pco node forget node3`, and a new token enrols it again.
[Install the appliance](appliance-install.md#on-a-cluster) has the install of the controller.

## Check

On the controller:

```sh
pco node list
pco cluster status
pco doctor --cluster
```

The node is `active`, `heard ... ago, watching`, and its replica `ready` in `pco cluster status`;
`pco doctor --cluster` has an `answer on node3` that is fine, and no failure in `agent channel`,
`clock skew` or `node versions`. On `node3`, `pco status` names the controller and says its agent
`hears the controller`.

## Undo

[Remove a node](remove-a-node.md): drain it, then `pco uninstall` on it, or for an agent
container `pco appliance uninstall --vmid <vmid>` on its node and `pco node forget` on the
controller.

## Read on

- [Run pco on a cluster](clusters.md): the cluster as a whole.
- [Architecture](../architecture.md#the-nodes): the states of a node and its admission.
- [Security](../security.md#join-tokens): join tokens, and what the enrolment protects.
- Reference: [pco setup](../cli/pco-setup.md), [pco node admit](../cli/pco-node-admit.md),
  [pco cluster join-token](../cli/pco-cluster-join-token.md),
  [pco appliance install](../cli/pco-appliance-install.md).
