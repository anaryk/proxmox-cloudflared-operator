# Install the appliance

## When you need it

You want pco on a Proxmox VE node without installing anything on the node itself: pco and its
connectors in a container of their own, on a node alone or on every node of a cluster
([On a cluster](#on-a-cluster)). [Appliance](../appliance.md) says what that gives and what it
costs against the host profile.

## Before you start

- Proxmox VE 8.4 or later, or 9.x, on amd64 or arm64, and root on the node.
- A storage that holds containers, ideally one that can snapshot (ZFS, LVM-thin, Ceph), so
  that backups of the appliance can run in snapshot mode.
- The bridge, and on a VLAN-aware bridge the VLAN, that the guests you will publish are on.
  The appliance reaches guests on its own segment, and reaches the Proxmox API at the node's
  address on that segment unless you name another with `--api-host`.
- An address for the appliance: a static one, or DHCP, ideally with a reservation. Its web
  interface follows the address of its card, so a reservation keeps it where you expect it.
- Outbound access from that segment to Cloudflare's API on port 443 and its edge on port 7844,
  TCP and UDP, and to Debian's mirrors for the security updates.
- A Cloudflare token as [Cloudflare token](../cloudflare-token.md) describes, in a file only
  root can read, or added later.
- No principal but the admins holds a privilege that reaches into the appliance, on it, on its
  pool or above; [who may reach the appliance](../appliance.md#who-may-reach-the-appliance)
  lists them. The installer names any it finds.

## Steps

1. Run the installer on the node with the appliance profile, the storage and, if you have it,
   the token:

       curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash -s -- --appliance --storage local-zfs --ip 192.0.2.120/24,gw=192.0.2.1 --vmid 120 --cf-token-file /root/cf-token

   To check the release key the script carries before it runs, take the script from the tag of
   the release as
   [SECURITY.md](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/SECURITY.md#with-the-installer)
   shows, and run that copy with the same arguments.

   On a VLAN-aware bridge add `--vlan`, as `--vlan 20`. The installer prints what it found
   first, then what it makes, and ends with the next steps and the fingerprint of the web
   interface's certificate. Keep the fingerprint.

2. Answer its questions. It asks whether to register the gate tags (yes), and only when it
   finds them, whether to go on beside another install of pco and whether to add `NoAccess`
   lines for principals that could reach into the appliance. Read what each line takes away
   before you say yes.

3. If you gave no token, add one:

       pct exec 120 -- pco credential add --label main

4. Tag a guest on the appliance's segment with `cf-tunnel` and write its routes into its Notes,
   as in [Quickstart](../quickstart.md#tag-a-guest-and-write-its-routes).

5. Acknowledge the segment: the last step of the setup in the web interface does it together
   with Start publishing, and the Networks page has it for later. On the command line, before or
   after the routes show up:

       pct exec 120 -- pco routes
       pct exec 120 -- pco segment acknowledge vmbr0

   Use `vmbr0:20` for VLAN 20 of `vmbr0`.

6. Approve the guests that wait, after reading why:

       pct exec 120 -- pco guest list
       pct exec 120 -- pco guest approve qemu/132

7. Look at the plan, and publish:

       pct exec 120 -- pco plan
       pct exec 120 -- pco apply

## On a cluster

On a cluster the appliance comes in kinds: a **controller** container, which serves the node it
runs on as a combined appliance does and serves the agent endpoint, TCP 8644 on its card's
address, and an **agent** container on each other node, which serves that node. The steps above
make a `combined` appliance, the kind for a node alone.

1. **The controller.** On the node that is to run it, with `--kind controller`. Add `--ha` and
   `--ha-nodes` to make it a Proxmox HA resource, after reading
   [Run the appliance's controller as an HA resource](appliance-ha.md):

       pco appliance install --kind controller --storage local-zfs --vmid 120 --cf-token-file /root/cf-token --checksums checksums.txt

   Its container's firewall gets the ipset `pco-agents` and one rule that lets those addresses,
   and no others, reach TCP 8644. A combined appliance that runs already becomes the controller in
   place, with its connectors running throughout: `pco appliance convert --vmid 120` on its node.
   Then go through steps 3 to 7 above on the controller.

2. **A join token for each other node**, made on the controller into a file only root reads,
   good once, for that node, for 15 minutes:

       pct exec 120 -- pco cluster join-token --node node2 --file /root/node2.join
       pct pull 120 /root/node2.join /root/node2.join

   Copy it to `node2` as root, and remove the copies you no longer need.

3. **The agent**, on `node2`, with the token:

       pco appliance install --kind agent --storage local-zfs --vmid 121 --join-file /root/node2.join --checksums checksums.txt

   `--join -` reads the token from standard input instead. It is never an argument, an
   environment variable or a line of the journal: the installer pushes it onto the agent's state
   volume with `pct push`, and the agent removes it once the controller answered. The agent's
   token holds the reading role `PCO` and nothing more. The agent makes its key and joins: the
   controller checks the token and the container, an LXC on `node2` in pool `pco`, no HA
   resource, with the MACs and the address it said, and records the node as `joining`.

4. **Admit it** by the fingerprint of its certificate, which the agent shows:

       pct exec 121 -- pco agent status
       pct exec 120 -- pco node list
       pct exec 120 -- pco node admit node2 --fingerprint 9f86d081

   `pco node list` on the controller shows every use of the join token; admit only when they are
   yours. A token used twice, or by a container that is not what it said, holds the node
   `contested` until `pco node forget node2`; a new token enrols it again.

Check it with `pct exec 120 -- pco cluster status` and `pct exec 120 -- pco doctor --cluster`:
every agent `active`, its replica `ready`, and the checks `agent container <node>` fine.
[Appliance](../appliance.md#a-cluster-of-appliances) has the whole of it, and
[Add a node](add-a-node.md#steps-an-appliance-agent) the agent of a node added later.

## Check

    pct exec 120 -- pco status
    pct exec 120 -- pco doctor

`pco status` shows `Profile: appliance` and `Identity: ok (lxc/120 on node1)`, and once applied
the tunnel with its connector ready. `pco doctor` runs the checks of the appliance besides the
usual ones. A fresh install is not all `✓`, and a `✗` is what to look at first. Two warnings
are usual:

- `quic buffer`: `net.core.rmem_max` of the node is under 7500000, the receive buffer
  `cloudflared` asks for its QUIC connections, and a node has less unless someone raised it.
  The container cannot set it. On the node:

      echo net.core.rmem_max=7500000 >> /etc/sysctl.d/90-pco.conf
      sysctl --system

- `admission`: the admission of the settings is `tag`, the default, and the guest of step 4
  carries the gate tag, so whoever may clone it makes a tagged guest of their own. Set
  `admission` to `approve` in the [settings](../settings.md), unless only admins hold
  `VM.Clone` on the tagged guests.

[Problems](../problems.md#doctor-checks) says what each check means.

Open `https://192.0.2.120:8643/`, compare the fingerprint the browser shows with the one the
installer printed, and sign in with a Proxmox VE user that holds `Sys.Modify` on `/`.

## Undo

On a cluster, remove the agent containers first, each with `pco appliance uninstall --vmid
<vmid>` on its node and `pco node forget <node>` on the controller, then the controller as below.

On the node, with the release the appliance runs:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | PCO_VERSION=1.2.3 bash -s -- --appliance --uninstall --vmid 120

It runs `pco appliance uninstall`, which lists what it removes and asks, and asks again what
becomes of the tunnel and the records at Cloudflare; [Uninstall](../uninstall.md#the-appliance)
has the details. A run of the installer
that failed has taken back what it made already; one that was killed is finished or taken back
with `PCO_RESUME` (see [the appliance](../appliance.md#a-run-that-was-cut-short)).
