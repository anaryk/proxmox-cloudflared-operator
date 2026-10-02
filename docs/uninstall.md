# Uninstall

`pco uninstall` takes pco off a node: the daemon, the connectors, the egress filter, what
`pco setup` created in Proxmox, and the store. It does that in an order that keeps what the
next part needs, and it removes only what pco made. Run it as root on the node, before you
remove the package.

## The command

    pco uninstall

It looks first. It reads what is on the node and what the install has at Cloudflare,
lists all of it, and asks once, with the answer defaulting to no:

    Remove pco from this node? [y/N]

Two more questions come on their own, because they reach beyond the node:

- If the install has Cloudflare credentials, it lists the DNS records and tunnels that the
  install has at Cloudflare and asks whether to delete them as well.
- If `pco setup` installed `cloudflared`, it asks whether to remove the package and its apt
  source.

Without a terminal, the questions have no one to answer them, and the command says so:
`stdin is not a terminal: pass --yes to confirm`. The flags answer them in advance.

| Flag | Effect |
|---|---|
| `--yes`, `-y` | Answers the first question and no other. An install that has Cloudflare credentials needs `--purge-cloudflare` or `--keep-cloudflare` as well, and `cloudflared` stays unless `--remove-cloudflared` is given. |
| `--purge-cloudflare` | Deletes the DNS records and the tunnel of the install at Cloudflare. |
| `--keep-cloudflare` | Leaves them as they are, without looking at them. |
| `--remove-cloudflared` | Removes the `cloudflared` package and its apt source, when setup installed them. |

`--purge-cloudflare` and `--keep-cloudflare` do not go together. With `--yes`, an install
that has Cloudflare credentials needs one of the two, and the command refuses without it:

    install 7f3a9c0d41b2 may have DNS records and a tunnel at Cloudflare, and the uninstall
    removes the credentials that reach them: say what becomes of them with
    --purge-cloudflare, which deletes them, or --keep-cloudflare, which leaves them, and
    then nothing on this node can remove them later

The point is that no default may leave objects behind that nothing can remove later. Once the
store is gone, so are the credentials, and with them the means to delete what the install
made at Cloudflare.

A script that removes everything looks like this:

    pco uninstall --yes --purge-cloudflare --remove-cloudflared

## What it removes, and in which order

1. **The daemon.** It stops and disables `pco.service`, so that it does not make again what
   is removed. A daemon that someone started by hand cannot be stopped by systemd, and it
   holds the lock of the node: the command then stops before it has removed anything, and
   says so.
2. **DNS records at Cloudflare** (with `--purge-cloudflare` or yes to the question): the
   records that carry the marker of this install, in every zone that a stored credential
   sees.
3. **The connectors.** Each `pco-cloudflared@<tunnel id>.service` is stopped and disabled
   and its files in `/var/lib/pco/tunnels` are removed. They stop before their tunnels are
   deleted, because Cloudflare refuses to delete a tunnel that still has a connection.
4. **Tunnels at Cloudflare** (with purge): the tunnel `pco-<install id>`, and any test tunnel
   that a credential check left behind.
5. **The egress filter.** It stops and disables `pco-egress.service` and deletes the
   nftables table `inet pco_egress`.
6. **What setup created in Proxmox**, as far as the manifest lists it:
   - the grant of the role `PCO` on `/` to `pco@pve`;
   - the token `pco@pve!pco`;
   - the user `pco@pve`;
   - the role `PCO`, but only if setup created it, it still holds exactly what setup gave it,
     and nobody else holds it. If you added a privilege or took one away, or gave the role to
     another user, group or token, the role stays and the command says why;
   - the registered tags that setup added, `cf-tunnel` and `cf-tunnel-managed`. Tags that
     were registered before stay in their order.
7. **`cloudflared`** (with `--remove-cloudflared` or yes to the question, and only if
   setup installed it): the package, its apt source `/etc/apt/sources.list.d/cloudflared.sources`
   if it is still as setup wrote it, and the key `/usr/share/keyrings/cloudflare-main.gpg`,
   which stays when another apt source names it.
8. **The store.** The node is removed from the registry, and then `/etc/pve/pco`,
   `/etc/pve/priv/pco` and `/var/lib/pco` go, with everything in them: the credentials, the
   connector tokens, the manifest, the event log, and the snapshots of adopted records.

When it is done it says:

    pco is removed from this node; the package goes with apt-get purge pco

A part that fails is reported and the others go on. The store is then kept, with the
credentials and the manifest, so that `pco uninstall` run again finishes the rest, and the
command exits with 1. The same holds when Cloudflare cannot be listed: the store is kept,
and you finish with `pco uninstall --purge-cloudflare` once Cloudflare answers, or say
`--keep-cloudflare`.

## What it never touches

- The guests: their tags, their Notes and their configuration. pco never wrote to them.
- Anything in Proxmox that the manifest does not list: a role, user or token that was there
  before setup, tags that were registered before, and privileges that others hold.
- DNS records without the marker of this install, tunnels of other names, and the objects of
  another install.
- `cloudflared`, when setup did not install it or `--remove-cloudflared` was not given.
- The package itself, the `pco-connector` user that the package's `sysusers.d` file makes,
  and `nftables`.
- Records that `pco adopt` replaced. A purge deletes pco's record, and the record it replaced
  does not come back. The snapshot of the old record is in `/etc/pve/pco/adopted.jsonl`,
  which the uninstall removes with the store: copy the file first if you may want it.

## Removing the package

After `pco uninstall`, remove the package:

    apt purge pco

Do it in this order. A plain `apt remove pco` stops and disables the daemon and leaves the
connectors running, so that removing the package does not take the tunnels down; the package
says so when it does, because only `pco uninstall` removes them and it needs `pco` to be
installed. `apt purge` removes `/var/lib/pco`, which holds the connector tokens and the
manifest. If you purge first, you have lost the means to see what setup created, and the
connectors are still running without anything to manage them.

A plain `apt remove` keeps `/var/lib/pco`, so that `pco setup` can bring the installation back
after a reinstall. It never touches `/etc/pve`.

## After a lost store

The store is lost when `/etc/pve/pco` is gone and the Cloudflare side is not: the cluster
filesystem was rebuilt, or the node was reinstalled. The tunnel `pco-<id>`, its records and
probably its connector are still there, and nothing on the node knows them.

If you run `pco setup` again it makes a new install with a new id and a new tunnel, and never
touches objects that carry another id. The old connectors on the node are reported in
`pco status`:

    connector for tunnel <tunnel id> belongs to install <old id>; pco setup --recover adopts
    that install, pco uninstall on this node removes it

To take the old install back, give the token that sees its tunnel to `--recover`:

    pco setup --recover --cf-token-file /root/cf-token

Recovery looks for tunnels named like those of pco in every account the token sees. If it finds
one install it adopts it. If it finds several it lists them and you choose with
`--install-id <id>`. It then makes the install id of the store that id, and gives the writer
identity a generation above the highest one found in the tunnels' configurations, so that the
daemon is not taken for a stale or foreign writer. It refuses when the token sees no tunnel of
an install of pco, when a configuration cannot be read, or when the store already holds a
different install (`recovery never replaces an install`). It stops `pco.service` while it works,
and starts it again if the recovery fails. A recovered install is in observe-only mode, as every
new install is: look at `pco plan`, and run `pco apply` when you are satisfied.

To remove what a lost install left, adopt it first, because `pco uninstall` deletes only what
it can tie to the install id:

    pco setup --recover --cf-token-file /root/cf-token
    pco uninstall --purge-cloudflare

You can also delete the objects by hand in the Cloudflare dashboard: the tunnel named
`pco-<id>`, and the DNS records whose comment begins with `pco:<id>`.

If the Proxmox objects remain (the role `PCO`, the user `pco@pve`, its token), `pco setup`
finds them and reuses them. A token whose secret is not stored on the node is made anew,
since Proxmox shows a secret only once. The manifest is in `/var/lib/pco`: if that is gone
too, a later `pco uninstall` does not know that setup made these objects and leaves them in
Proxmox, and you remove them by hand with `pveum`.
