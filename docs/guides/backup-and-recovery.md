# Back up and recover

This guide says what of pco to back up and how, how to bring pco back after its store is lost
or the node is restored, from a bundle of `pco export` or from Cloudflare, what a restored guest
does, and what differs on a cluster.

## When you need it

- You set up backups of a node that runs pco.
- The store of pco is lost: the cluster filesystem was rebuilt, or the node was reinstalled.
- The node, or a guest, was restored from a backup.
- A cluster that ran pco was lost, and is built again.

## What to back up

Most of the state pco makes again by itself: it verifies the addresses again, settles the claims
again from the Notes, and reads the run tokens of the tunnels again from Cloudflare. What it
cannot make again is what an admin decided, and the secrets.

| What | Where | Made again by |
|---|---|---|
| The settings | `/etc/pve/pco/meta/settings.json` | Nothing: your values. |
| The approvals of guests | `/etc/pve/pco/approvals/` | `pco guest approve`. |
| The manual routes | `/etc/pve/pco/routes/` | `pco route manual add`. |
| The acknowledged segments | `/etc/pve/pco/segments/` | `pco segment acknowledge`. |
| The attached networks and the consents of the managed network | `/etc/pve/pco/networks/`, `/etc/pve/pco/consents/` | `pco network attach`, `pco managed approve`. |
| The records `pco adopt` replaced | `/etc/pve/pco/adopted.jsonl` | Nothing: the only copy. |
| The Cloudflare credentials | `/etc/pve/priv/pco/credentials/` | `pco credential add`, with the tokens. |
| What setup created | `/var/lib/pco/manifest.json` | Nothing; `pco uninstall` needs it to remove the role, user and token. |
| The block list of the egress filter | `/var/lib/pco/egress-blocked.json`, when there is one | `pco egress block`. |
| The web interface | `/etc/default/pco-web`, and a certificate of your own in `/etc/pco/web/` | `pco setup`, `pco web cert import`. |

[Files](../files.md) lists every path, with whether to back it up. The install id itself needs no
backup: `pco setup --recover` takes it from the tunnels at Cloudflare.

### The bundle

[`pco export`](../cli/pco-export.md) writes all of this, and everything else the controller keeps
in the store, into one encrypted file: the install with the cluster it was made in, the settings,
approvals, manual routes and acknowledged segments, the registry of nodes, the CA with its key
(and the previous CA while a rotation runs), `leader.json`, the networks and service addresses,
the leases, consents and attachments of the managed network, the claims, tombstones, placements
and moves, and with `--credentials` the Cloudflare tokens. [`pco import`](../cli/pco-import.md)
restores it on a node where pco is not set up, in observe-only mode.

Two things are not in it. The run tokens, bindings and caches, which the daemon reads or builds
again, and the engine memory, what the controller learns between its runs: it is learnt again in
the observe-only window after a restore, before anything is removed. A bundle written by an
earlier release carries no pool of service addresses either; the restored pool then starts above
the addresses in use, and an address that had been released may be given out again.

## Before you start

- You are root on the node, and have `jq` (`apt install jq`).
- A backup that holds `/etc/pve/priv/pco` holds the Cloudflare tokens in the clear: keep it as
  you keep the tokens.

## Back up

1. Write the bundle, on the controller or, on a host, on any node, and copy it off the cluster.
   With `--credentials` it holds the Cloudflare tokens too; keep the passphrase somewhere else
   than the file:

   ```sh
   pco export --credentials /root/pco.bundle
   ```

   ```text
   Wrote /root/pco.bundle.
   install                 7f3a9c0d41b2
   manual routes           1
   approvals               3
   acknowledged segments   0
   nodes                   3
   CA and its key          yes
   controller objects      12
   Cloudflare credentials  1
   ```

   An empty passphrase at the question has pco draw one and show it once. Write a new bundle
   after every change you would not like to make again, and before `pco cluster finish-upgrade`.
   The steps below keep the same in a form you can read and apply by hand.

2. Save the decisions as the commands print them, in the form the commands take back:

   ```sh
   mkdir -m 700 -p /root/pco-export
   pco settings show --json > /root/pco-export/settings.json
   pco guest list --json > /root/pco-export/approvals.json
   pco route manual list --json > /root/pco-export/manual-routes.json
   pco segment list --json > /root/pco-export/segments.json
   pco credential list > /root/pco-export/credentials.txt
   ```

3. Save the files, as they are:

   ```sh
   tar --ignore-failed-read -czf /root/pco-backup.tar.gz \
     /etc/pve/pco /etc/pve/priv/pco /var/lib/pco/manifest.json /var/lib/pco/egress-blocked.json \
     /etc/default/pco-web /etc/pco/web /root/pco-export
   chmod 600 /root/pco-backup.tar.gz
   ```

   `--ignore-failed-read` lets the files that a node does not have be missing. Copy the archive
   off the node, to where your other backups of it go. A backup of the node that holds the
   cluster filesystem has the first two directories too.

## Recover after a lost store

The store is lost when `/etc/pve/pco` is gone and the tunnel and the records of the install are
still at Cloudflare. `pco setup --recover` finds the install by its tunnels, with a token that
sees them, and makes it the install of the node again. [Uninstall](../uninstall.md#after-a-lost-store)
says what it checks and when it refuses.

1. Recover, with a token that sees the tunnel of the install:

   ```sh
   pco setup --recover --cf-token-file /root/cf-token
   ```

   Among its lines:

   ```text
   store: recovered install 7f3a9c0d41b2 with writer generation 2; it only observes until pco apply
   credentials: stored the token as credential c3d4e5f6 (setup)
   ```

   The writer gets a generation above every one at Cloudflare, so that the daemon is taken
   for neither a stale nor a foreign writer. The recovered install only observes.

2. Add the other tokens, if there were others: `pco credential add --label <label>` for each.
   `/root/pco-export/credentials.txt` has their labels. The credentials have new ids.

3. Put the settings back. Take the saved settings over the ones there, in observe-only mode,
   and without the zone pins, whose credential ids are new:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq --slurpfile old /root/pco-export/settings.json \
     '.settings = ($old[0].settings | .observeOnly = true | del(.zonePins))' \
     /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

   Pin the zones again with the new ids, as in
   [Use several tokens and accounts](several-tokens-and-accounts.md#steps). If the command
   says that a setting is read only at start, `systemctl restart pco`.

4. Approve the guests again, make the manual routes again with their ids, and acknowledge the
   segments again:

   ```sh
   for owner in $(jq -r '.[].owner' /root/pco-export/approvals.json); do pco guest approve "$owner"; done
   jq -r '.[] | select(.acknowledged) | .bridge + (if .vlan then ":\(.vlan)" else "" end)' /root/pco-export/segments.json
   pco segment acknowledge vmbr0
   pco route manual add status.example.com --address 10.0.5.20 --port 9000 --id status
   ```

   An approval is of the identity a guest has now, and pco shows it before it records it,
   with the soft-denied addresses the guest is at now. An address that was allowed with
   `--allow-address` and that the guest is not at now is under `addresses` in
   `approvals.json`: give it again, `pco guest approve <owner> --allow-address <address>`.
   `manual-routes.json` has the hostname, target and options of each manual route.

5. Read `pco plan` and `pco status`, and when they are what you expect:

   ```sh
   pco apply
   ```

Without a token that sees the tunnel, there is nothing to recover from: `pco setup` makes a
new install with a new id, and the tunnel and records of the old one stay at Cloudflare until
you delete them there.

## Restore from a bundle

[`pco import`](../cli/pco-import.md) restores the install of a bundle on a node where pco has no
install, with the daemon stopped, and before `pco setup`, which keeps the install it finds. The
restored install observes until `pco apply`, and its `leader.json` is the one of the bundle, so
the daemon writes nothing until `pco promote` draws an epoch above every sentinel at Cloudflare
and every epoch an agent remembers.

**The store lost, the cluster kept**, as after `/etc/pve/pco` was removed by mistake:

1. Stop pco on every node; the connectors run on:

   ```sh
   systemctl stop pco
   ```

2. On one node, restore the install:

   ```sh
   pco import /root/pco.bundle
   ```

   ```text
   Restored the install from /root/pco.bundle.
   install                 7f3a9c0d41b2
   manual routes           1
   approvals               3
   acknowledged segments   0
   nodes                   3
   CA and its key          yes
   controller objects      12
   Cloudflare credentials  1
   pco only observes until pco apply.
   Next: run pco setup if this node has none, then run pco promote here; in a new cluster,
   set the other nodes up only after the promotion.
   ```

3. Start pco on every node again, `systemctl start pco`, and on the node that takes the lease,
   which `pco status` names, read what differs and promote it:

   ```sh
   pco promote
   ```

   The agents remember a later epoch than the bundle's, and the promotion draws above it.
4. `pco credential add` for each token the bundle did not carry, then read `pco plan`, and
   `pco apply`.

**A cluster lost whole, built again.** The bundle records the cluster it was made in, by the
fingerprint of the cluster CA of Proxmox VE, and a new cluster has another. The first node that
`pco setup` runs on takes the store into the new cluster; nothing the nodes of the lost cluster
hold is trusted after that, and every other node enrols again.

1. Make sure nothing of the lost cluster runs the install any more. Setup looks at Cloudflare and
   refuses while a connector of the install is connected there:

   ```text
   setup step cluster: the cluster the bundle was made in still runs install 7f3a9c0d41b2, or a copy of it does: at Cloudflare tunnel pco-7f3a9c0d41b2 has connector 5d1c... connected from 203.0.113.7. ...
   ```

   If a node of the old cluster is still up, stop pco and its connectors there,
   `systemctl stop pco 'pco-cloudflared@*'`, and wait until Cloudflare shows no connection.

2. On the node that is to run the controller, install the package without the setup
   (`PCO_SKIP_SETUP=1`), and restore the install:

   ```sh
   pco import /root/pco.bundle
   ```

3. Set the node up. A bundle written without `--credentials` has no Cloudflare token to look with:
   give one, which setup also stores.

   ```sh
   pco setup --cf-token-file /root/cf-token
   ```

   Among its lines:

   ```text
   store: install 7f3a9c0d41b2 kept, restored from a bundle made on 2026-10-01
   cloudflare: no connector of install 7f3a9c0d41b2 is connected
   cluster: took the store of cluster lab into this one (4be1...) under a new CA; the daemon writes nothing until pco promote here makes this cluster its writer, and the other nodes are set up after it
   ```

   Setup makes a new CA, so that no certificate of the old nodes is accepted, and marks every
   node of the lost cluster `removed`; this node, and later each node of the old name, keeps the
   index the name had, so the MACs of its legs stay. The daemon starts and writes nothing:
   `pco status` says `this store was made in cluster lab (<fingerprint>); pco promote here makes
   this cluster its writer`.

4. Promote the node. pco asks you to type the name of the cluster the bundle was made in, or its
   fingerprint when it has none, as the differences show it:

   ```sh
   pco promote
   ```

   The promotion makes the new cluster the store's and draws an epoch above every sentinel at
   Cloudflare, in observe-only mode.

5. Set the other nodes up, each with `pco setup`. They join as `joining`, with certificates of the
   new CA, and the controller admits them. A node set up before the promotion is refused, and
   nothing is changed on it: `... and node pve1 took it into this cluster: run pco promote on pve1
   first, then pco setup here`.

6. `pco credential add` for each token the bundle did not carry, then read `pco plan`, and
   `pco apply`. If the disks of the lost cluster may come back, end the run tokens they hold once
   pco applies: `pco tunnel rotate`.

**A node alone, reinstalled**: a reinstalled node has a new cluster CA of Proxmox VE, so the
store counts as one of another cluster, and the steps are those of a cluster lost whole, with one
node: install the package without the setup (`PCO_SKIP_SETUP=1`), `pco import /root/pco.bundle`,
`pco setup`, which takes the store under a new CA, and `pco promote`, which asks for the fingerprint
of the node's old CA, as the differences show it.

**The appliance**: `pco import` restores the install of a host only. An appliance's controller
whose state volume is lost is adopted from Cloudflare with `pco appliance repair --recover`, as
[Restore the appliance from a backup](appliance-restore.md) describes.

## After a restore of the node

A node restored from a backup brings back the store as it was then, and the connectors in
`/var/lib/pco` with it.

1. Put the role, the user, the token and the tags back in Proxmox, as a restore can leave them
   out of step:

   ```sh
   pco setup --repair
   ```

2. Read the `Writer:` line of `pco status`. When a recovery took a newer generation after the
   backup was made, the restored copy finds a sentinel newer than anything it knows, says
   `foreign`, and writes nothing. Take a generation above it:

   ```sh
   pco setup --recover --cf-token-file /root/cf-token
   pco apply
   ```

   Never run a restored copy of the store beside the original: the two cannot be told apart,
   and each reports the other's connector as one it does not run.

The approvals, manual routes and settings are those of the backup; anything decided since is to
be decided again.

On a cluster a node restored from a backup gets the cluster filesystem from the other nodes, not
from its backup: only its own state in `/var/lib/pco` is old. It issues its certificate again if
it must, joins as `joining`, and is admitted as after a reboot. A copy of `/etc/pve/pco` restored
under a running cluster is another matter: the controller finds the store older than what the
agents remember and writes nothing; [the runbook](cluster-runbooks.md#behind-after-a-restore)
says what to do.

## A restored guest

- **Restored to its own VMID**, it is the same owner: it keeps the hostnames of that VMID, and
  publishes what its Notes name, without anyone handing them over. It keeps its approval while
  it keeps its identity, which a restore with new unique addresses (`--unique` of `qmrestore`
  and `pct restore`) changes; then it waits for an approval again in admission mode `approve`.
- **Restored to another VMID**, beside the original, it is another owner: its routes are in
  `conflict` and serve nothing while the original holds the hostnames. If it runs with the MAC
  of the original, its addresses are refused, `MAC <MAC> is also configured on qemu/<id>`.
  [Move a hostname to another guest](move-a-hostname.md) hands a hostname over when the copy is
  to take over.

## On a cluster

There is no pco to move between the nodes of a cluster: it runs on every node, and the lease
moves by itself. A node that is lost for good is forgotten, `pco node forget <node>`, and the
store stays on the cluster filesystem of the others ([Remove a node](remove-a-node.md)). A
standalone node that runs pco and is to join a cluster loses its `/etc/pve` with `pvecm add`:
write a bundle first, stop pco and its connectors there (`systemctl stop pco 'pco-cloudflared@*'`),
and import the bundle into the cluster as for [a cluster lost whole](#restore-from-a-bundle).

## Check

```sh
pco status
pco doctor
pco guest list
pco route manual list
pco segment list
```

`pco status` is in `Mode: enforce`, `Writer: ok`, with its tunnels verified, and the lists show
what you saved.

## Undo

A recovery needs no undo: it only adopts what is at Cloudflare, or what the bundle holds, and
starts in observe-only mode.

## Read on

- [Files](../files.md#what-to-back-up): what to back up, in the reference.
- [Uninstall](../uninstall.md#after-a-lost-store): a lost store, in full.
- [Architecture](../architecture.md#one-writer): the writer identity, and why two copies of a
  store must not run side by side.
- Reference: [pco export](../cli/pco-export.md), [pco import](../cli/pco-import.md),
  [pco promote](../cli/pco-promote.md), [pco setup](../cli/pco-setup.md),
  [pco uninstall](../cli/pco-uninstall.md).
