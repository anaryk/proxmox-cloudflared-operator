# Back up and recover

This guide says what of pco to back up and how, how to bring pco back after its store is lost
or the node is restored, what a restored guest does, and how to move pco to another node of a
cluster.

## When you need it

- You set up backups of a node that runs pco.
- The store of pco is lost: the cluster filesystem was rebuilt, or the node was reinstalled.
- The node, or a guest, was restored from a backup.
- pco should run on another node, because the one it runs on is retired or lost.

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
| The records `pco adopt` replaced | `/etc/pve/pco/adopted.jsonl` | Nothing: the only copy. |
| The Cloudflare credentials | `/etc/pve/priv/pco/credentials/` | `pco credential add`, with the tokens. |
| What setup created | `/var/lib/pco/manifest.json` | Nothing; `pco uninstall` needs it to remove the role, user and token. |
| The block list of the egress filter | `/var/lib/pco/egress-blocked.json`, when there is one | `pco egress block`. |
| The web interface | `/etc/default/pco-web`, and a certificate of your own in `/etc/pco/web/` | `pco setup`, `pco web cert import`. |

[Files](../files.md) lists every path, with whether to back it up. The install id itself needs no
backup: `pco setup --recover` takes it from the tunnels at Cloudflare.

## Before you start

- You are root on the node, and have `jq` (`apt install jq`).
- A backup that holds `/etc/pve/priv/pco` holds the Cloudflare tokens in the clear: keep it as
  you keep the tokens.

## Back up

1. Save the decisions as the commands print them, in the form the commands take back:

   ```sh
   mkdir -m 700 -p /root/pco-export
   pco settings show --json > /root/pco-export/settings.json
   pco guest list --json > /root/pco-export/approvals.json
   pco route manual list --json > /root/pco-export/manual-routes.json
   pco segment list --json > /root/pco-export/segments.json
   pco credential list > /root/pco-export/credentials.txt
   ```

2. Save the files, as they are:

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

   An approval is of the identity a guest has now, and pco shows it before it records it.
   `manual-routes.json` has the hostname, target and options of each manual route.

5. Read `pco plan` and `pco status`, and when they are what you expect:

   ```sh
   pco apply
   ```

Without a token that sees the tunnel, there is nothing to recover from: `pco setup` makes a
new install with a new id, and the tunnel and records of the old one stay at Cloudflare until
you delete them there.

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

## Move pco to another node

pco runs on one node of a cluster, and the store, which the cluster filesystem shares, names it.
To move pco from `node1` to `node2`, the store is taken down on `node1` and the install is
recovered on `node2`. The published hostnames are down from the moment `node1` stops its
connectors until `pco apply` on `node2`: Cloudflare's error 1033 for every one.

1. Back up on `node1`, as above.
2. Make sure `node2` has what `node1` had: an address of its own on the bridges and VLANs of
   the published guests, and the way out to Cloudflare. Install the package on `node2` without
   setting it up, as [Quickstart](../quickstart.md#install) describes with `PCO_SKIP_SETUP=1`.
3. On `node1`, take pco down and leave Cloudflare as it is:

   ```sh
   pco uninstall --yes --keep-cloudflare
   ```

   This stops the connectors and removes the store, which lets `node2` take the install, and
   what setup made in Proxmox, which `node2` makes again.
4. On `node2`, recover the install, then put back the decisions and start publishing as in
   [Recover after a lost store](#recover-after-a-lost-store), steps 2 to 5:

   ```sh
   pco setup --recover --cf-token-file /root/cf-token
   ```

When `node1` is lost for good, the store it left is still on the cluster filesystem and names
it, and `pco setup` on `node2` refuses: `pco is already set up on node node1; cluster support
arrives in a later release`. Save `/etc/pve/pco` and `/etc/pve/priv/pco` first, which every node
of the cluster has, with `tar -czf /root/pco-node1-store.tar.gz /etc/pve/pco /etc/pve/priv/pco`:
the settings are the object in `data` of `/etc/pve/pco/meta/settings.json`, and the approvals,
manual routes and segments are files of their own. Then, on `node2` with the
package installed, `pco uninstall --yes --keep-cloudflare` removes that store, and nothing else,
since `node2` has no manifest of its own. Recover as in step 4. The role and the user that
`node1` made stay in Proxmox, and setup on `node2` uses them; it makes the token anew, as its
secret went with the store. If `node1` comes back with its
disk as it was, its connectors start and serve the tunnel beside those of `node2`: `pco status`
on `node2` names them as connectors that pco does not run, and
[Rotate the tunnel token](rotate-the-tunnel-token.md) cuts them off. Reinstall `node1` before it
joins the cluster again.

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

A recovery needs no undo: it only adopts what is at Cloudflare and starts in observe-only mode.
A move is undone by the same move in the other direction.

## Read on

- [Files](../files.md#what-to-back-up): what to back up, in the reference.
- [Uninstall](../uninstall.md#after-a-lost-store): a lost store, in full.
- [Architecture](../architecture.md#one-writer): the writer identity, and why two copies of a
  store must not run side by side.
- Reference: [pco setup](../cli/pco-setup.md), [pco uninstall](../cli/pco-uninstall.md).
