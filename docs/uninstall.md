# Uninstall

`pco uninstall` takes pco off a node: the daemon, the connectors, the egress filter, what
`pco setup` created in Proxmox, and the store. It does that in an order that keeps what the
next part needs, and it removes only what pco made. Run it as root on the node, before you
remove the package. An appliance is removed with `pco appliance uninstall` instead; see
[the appliance](#the-appliance).

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
3. **The connectors.** Each `pco-cloudflared@<tunnel id>.service` on the node, whichever
   install made it, is stopped and disabled and its files in `/var/lib/pco/tunnels` are
   removed. They stop before their tunnels are deleted, because Cloudflare refuses to delete
   a tunnel that still has a connection. The tunnel of another install stays at Cloudflare.
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
filesystem was rebuilt, or the node was reinstalled. The tunnel `pco-<id>` and its records
are still at Cloudflare, and if `/var/lib/pco` survived, so are its connectors on the node.
Nothing on the node knows them any more. What `pco setup` does then depends on those
connectors.

If connectors of the old install are still on the node, a plain `pco setup` refuses before
it creates anything, because a new install would never prune them:

    setup step store: the store holds no install, but this node runs connectors of install
    7f3a9c0d41b2 (2 connectors), which a new install would never prune: run pco setup
    --recover to adopt that install, or pco uninstall --keep-cloudflare to remove pco from
    this node and start over; pco setup --new-install starts a new install beside them

(With connectors of several installs it names each one, and `--recover` then needs
`--install-id`.) There are three ways on:

- **Adopt the old install**, which is usually what you want. Give `--recover` the token that
  sees its tunnel:

      pco setup --recover --cf-token-file /root/cf-token

- **Start over.** `pco uninstall --keep-cloudflare` removes the old connectors and whatever
  else of pco is on the node, and leaves Cloudflare as it is. Then `pco setup` makes a new
  install. The old install's tunnel and records stay at Cloudflare, and nothing on this node
  can remove them any more: delete them in the dashboard, the tunnel named `pco-<id>` and
  the DNS records whose comment begins with `pco:<id>`.
- **Go on beside them**, with `pco setup --new-install`. The old connectors keep running and
  the new install never prunes them; `pco status` reports each one as a problem. Use it
  only when you know why they are there.

If no connector of the old install is on the node, as after a reinstall that also lost
`/var/lib/pco`, a plain `pco setup` makes a new install with a new id, which only observes.
It creates its own tunnel after you run `pco apply`, and it never touches objects that carry
another id.

Recovery looks for tunnels named like those of pco in every account the token sees. If it
finds one install it adopts it. If it finds several it lists them and you choose with
`--install-id <id>`. It then makes the install id of the store that id, and gives the writer
identity a generation above the highest one found in the tunnels' configurations, so that the
daemon is not taken for a stale or foreign writer. The connectors of that install on the node
are then its own again. Recovery refuses when the token sees no tunnel of an install of pco,
when a configuration cannot be read, or when the store already holds a different install
(`recovery never replaces an install`). It stops `pco.service` while it works, and starts it
again if the recovery fails. A recovered install is in observe-only mode, as every new
install is: look at `pco plan`, and run `pco apply` when you are satisfied.

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

## The appliance

`pco appliance uninstall --vmid <vmid>` removes an [appliance](appliance.md) and what its
installer made for it. Nothing of pco is installed on the node, so `install.sh` runs it, from a
temporary directory, after it has checked the release; use the release the appliance runs:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | PCO_VERSION=1.2.3 bash -s -- --appliance --uninstall --vmid 120

The arguments after `--uninstall` go to `pco appliance uninstall`. It runs on the node that has
the container, and refuses on another node of the cluster, naming the right one.

### What it finds, and how

It removes only what carries the mark of the installer, and decides from the marks rather than
from what the appliance says: the description of the container
(`pco appliance vm<vmid>, installed <date> by pco appliance install`, with a line
`NoAccess for <principal> on <path>` for each `NoAccess` line the installer added), the comment
of the token (`pco appliance vm<vmid>`), the comment of the pool (`pco appliances`), the comment
of the user (`pco operator`), and for the roles, which have no comment, their names with exactly
the privileges pco gives them. It pulls the manifest `/var/lib/pco/manifest.json` out of a
running appliance for what carries no mark, the registered tags and the template, reads it as
untrusted input and checks every object it names against Proxmox; it does without the manifest
when it cannot have it. A `NoAccess` line goes only when the description of the container names
it, as the appliance could name any line in its manifest; one above the container that nothing
names is kept and listed, with the `pveum acl delete` command that takes it back. A container whose description lacks the mark is refused:
`lxc/<vmid> is not a pco appliance (its description lacks the mark of the installer): nothing was removed`.

It lists what goes and what stays, and why, and asks once:

    pco appliance uninstall removes from this node:
      container lxc/120 (running), with its state volume and the secrets on it
      Proxmox token pco@pve!vm120
      Proxmox user pco@pve
      Proxmox role PCO
      Proxmox pool pco
      the registered tags cf-tunnel, cf-tunnel-managed
      (the template local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst stays; --keep-template=false removes it)
      (what the install has at Cloudflare stays, and nothing on this node can remove it later)
    Remove the appliance lxc/120 and the objects above? [y/N] y
    container lxc/120: destroyed
    token pco@pve!vm120: removed
    user pco@pve: removed
    role PCO: removed
    pool pco: removed
    registered tags: removed cf-tunnel, cf-tunnel-managed
    the appliance lxc/120 is removed from this node

That run was given `--keep-cloudflare`.

### The order

1. **Cloudflare**, with `--purge-cloudflare` or yes to its question: the DNS records and the
   tunnel of the install are deleted through the running appliance, which holds the
   credentials. It needs the container running; a purge that fails removes nothing else.
2. **The container**, with its state volume and every secret on it: its protection is taken
   off, it is stopped and destroyed.
3. **The token**, before its user, as a user that goes leaves the secrets of its tokens behind.
4. **The network grants** of `pco appliance grant-network`, and the **`NoAccess` lines** the
   installer added, while they are as it made them. When the container could not be destroyed,
   its `NoAccess` lines stay, as they keep principals away from the secrets on its volume.
5. **The user `pco@pve` and the role `PCO`**, and the roles of the network grants, when nothing
   else uses them.
6. **The pool `pco`**, when it is empty.
7. **The registered tags** the installer added.
8. **The template** the installer downloaded, with `--keep-template=false` only. A template of
   pco that the manifest does not name as the installer's always stays.

A part that fails is reported and the rest goes on; running the uninstall again finishes it.

### What it keeps

- The user `pco@pve` and the role `PCO` while another token of the user uses them: a host
  install of pco (`pco@pve!pco`) or another appliance (`pco@pve!vm<vmid>`).
- The registered tags while a host install of pco on the node, or another appliance, uses them.
- The pool while it holds anything else.
- A `NoAccess` line above the container while another appliance is there, which it keeps the
  principal out of as well.
- An object that carries no mark, or whose mark is not this appliance's: the uninstall names it
  and why it stays.

### Cloudflare

The credentials that reach the install at Cloudflare are in the container, and go with it. So
the uninstall asks what becomes of the tunnel and the records, as `pco uninstall` does:
`--purge-cloudflare` deletes them, `--keep-cloudflare` leaves them. With `--yes`, an install
that has something at Cloudflare needs one of the two. A stopped container cannot be asked:
start it for a purge, or keep. What is left at Cloudflare is the tunnel `pco-<install id>` and
the DNS records whose comment begins with `pco:<install id>`, to delete in the dashboard.

A copy of the appliance beside its original, a clone or a restore while the original is still
there, is removed as a container, and what it reaches at Cloudflare, which is the original's,
always stays: `--purge-cloudflare` is refused for it.

A container that no node of the cluster has any more counts as gone, and the uninstall removes
what is left of it: the token, the grants, and the rest as above.
