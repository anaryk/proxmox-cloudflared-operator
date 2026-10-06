# Homelab

This page describes one Proxmox VE node at home, with one person who runs it and is its
only user, and what pco does there: which choices it asks for, which it does not, and what
the setup ends with.

## The situation

A node called `pve1` stands in a closet, on the home network 10.0.0.0/24. It runs a dozen
containers and a few virtual machines, and the owner is the only person with an account in
Proxmox. The domain `example.com` has its DNS at Cloudflare. A dashboard, a photo library
and a NAS should be reachable from outside. The router forwards no ports, and the owner does
not want to look after a `cloudflared` in every guest, or to keep a list of which of them
has which tunnel.

pco runs on the node. The dashboard and the photo library are guests with the tag
`cf-tunnel` and their hostnames in their Notes. The NAS is not a guest, so it is one manual
route that the owner makes with a command.

## The setup

A visitor reaches a hostname through Cloudflare's edge; the connector on the node, which the
egress filter confines to the addresses pco verified, carries the request to a container, to
a virtual machine and, for the NAS, to an address that is no guest's:

~~~mermaid
flowchart TB
    visitor["Visitor"] --> edge["Cloudflare edge"]
    subgraph lan["Home network, 10.0.0.0/24"]
        subgraph node["Proxmox VE node pve1, host profile"]
            connector["cloudflared connector"] --> filter["Egress filter"]
            daemon["pco daemon"]
            dash["lxc/110: dashboard, :3000"]
            photos["qemu/120: photo library, :3001"]
        end
        nas["NAS, 10.0.0.50:5000"]
    end
    edge -->|"through the tunnel the connector opened"| connector
    filter --> dash
    filter --> photos
    filter -->|"manual route"| nas
    daemon -. "tunnel configuration, DNS records" .-> edge
    daemon -. "verified targets" .-> filter
~~~

## Decisions

### Profile

The host profile. A root daemon on the node is the price, and at home the owner can decide to
pay it ([Security](../security.md#what-runs-with-which-rights) says what the daemon can do).
In return it is the profile that can prove an address at the level `port`. The appliance is
for a node that should hold nothing but Proxmox; see
[Profiles](../profiles.md#why-there-are-two).

### Admission

Leave `admission` at `tag`. The owner is the only one who can edit a guest, tag it or clone
it, so a guest that carries the gate tag is a guest the owner chose. The `admission` check of
`pco doctor` still warns while guests carry the tag, because pco does not read who may clone
a guest and cannot rule out that someone may. The warning does not change the exit status of
`pco doctor`. With one user it can stay. Setting `admission` to `approve` makes it go away
and makes every new guest wait for `pco guest approve`; that is a step with no one to guard
against here.

### Identity minimum

Leave `identityMinimum` at `port`. Every guest is on the node that runs pco, so every guest
can be proven at that level. If the home lab has more nodes, pco still runs on one, and the
guests of the others would be held back; the [company cluster](company-cluster.md) page
describes that, and the simplest answer at home is to keep the guests you publish on the
node that runs pco.

### Tokens

One Cloudflare token, with the three permissions of [Cloudflare token](../cloudflare-token.md#permissions)
on the account and on the one zone that is published in, `example.com`. A token that is
scoped to the zone cannot publish anything in another.

Whether to give it a time to live is a real choice at home. pco has no command that replaces
the secret of a credential, so rotating a token means adding a new one, pinning the zone to
it, revoking the old one and removing it ([the steps](../cloudflare-token.md#rotating-a-token)),
and a token that has expired stops every change at Cloudflare until it is replaced, though
what is published keeps serving. With a time to live, `pco status` and `pco doctor` warn 30
days before it ends. Without one there is nothing to remember and a token that does not
expire; the narrow scope is what limits it. Either is reasonable.

Give `pco setup` the token with `--cf-token-file`, or add it afterwards with
`pco credential add`, and prove that it may write before the first publish:
`pco credential check <id> --deep`.

### Egress filter

Nothing to decide. `pco setup` turns it on and the daemon keeps it in place. What a stolen
token could do is limited by it: a rule written at Cloudflare for the address of the node
gets a refused connection ([Security](../security.md#the-connector-egress-filter)). One
thing to look for on a fresh node: `pco doctor` warns while `nftables.service` is enabled,
because the ruleset it loads at start flushes the table of pco, and the connectors are not
confined until the daemon loads it again.

### Web interface access

The web interface listens on the node's own address, port 8643, and is reached from the home
network. Sign in with the Proxmox account of the owner. It is not published: a guest cannot
publish the address of the node, which is on [the denylist](../identity.md#the-denylist-and-the-node), so
neither the web interface of pco nor that of Proxmox is reachable through a route written in
a guest's Notes. Do not forward the port at the router. Reach it from outside through a VPN
if you need it.

## What it ends with

The settings differ from the defaults in one place, the address that a manual route may
point at, which is the NAS's:

~~~json
{
  "manualCIDRs": ["10.0.0.50/32"]
}
~~~

Put it in the `settings` object of the file that `pco settings show --json` prints and apply
it ([Settings](../settings.md#changing-the-settings)):

    pco settings show --json > settings.json
    pco settings apply settings.json

The Notes of the container `110` hold one line:

~~~text
```cf-tunnel
dash.example.com -> :3000
```
~~~

and those of the virtual machine `120` another:

~~~text
```cf-tunnel
photos.example.com -> :3001
```
~~~

Both guests carry the tag, as `pct set 110 --tags 'cf-tunnel'` and
`qm set 120 --tags 'cf-tunnel'` make it. The NAS is a route of its own:

    pco route manual add nas.example.com --address 10.0.0.50 --port 5000 --id nas

Until `pco apply`, nothing is published; `pco plan` shows what would be. Once it is applied,
`pco routes` lists the three:

    HOSTNAME            STATE   LEVEL   SERVICE                OWNER       ZONE         NOTE
    dash.example.com    active  port    http://10.0.0.21:3000  lxc/110     example.com  -
    nas.example.com     active  manual  http://10.0.0.50:5000  manual/nas  example.com  -
    photos.example.com  active  port    http://10.0.0.22:3001  qemu/120    example.com  -

The level of the NAS is `manual`: nothing proves that the address is the NAS's, and only the
denylist and a connection to the port are checked.

## What to watch

- **A clone of a published guest.** The owner clones the photo library to try an upgrade.
  The clone carries the tag and the Notes, so it asks for the same hostname, and does not
  get it: the first guest to claim a hostname keeps it. The original keeps serving, and the
  clone shows as a conflict:

      HOSTNAME            STATE     LEVEL   SERVICE                OWNER       ZONE         NOTE
      dash.example.com    active    port    http://10.0.0.21:3000  lxc/110     example.com  -
      nas.example.com     active    manual  http://10.0.0.50:5000  manual/nas  example.com  -
      photos.example.com  active    port    http://10.0.0.22:3001  qemu/120    example.com  -
      photos.example.com  conflict  -       -                      qemu/121    -            hostname is held by qemu/120

  Change the Notes of the clone, or take the tag off it. `pco claims list` says who holds
  what.
- **A reboot of the node.** Until the daemon's first cycle has verified the routes, a
  published hostname answers 502. A guest that Proxmox starts late is withdrawn until it
  runs and its address verifies, and its hostname answers 503 meanwhile
  ([Operations](../operations.md#the-daemon-and-its-units)).
- **The node itself.** pco runs on this one node and so do the connectors. While it is down,
  every published hostname is down, and nothing takes over.
- **Services that listen on 127.0.0.1 only.** pco proves an address of the guest, and a
  service that does not listen on it makes its route `unreachable`, with a note that the
  connection was refused.
- **The token.** `pco status` shows its expiry, and `pco doctor` warns 30 days before it.
- **Upgrades.** `apt upgrade` updates `cloudflared`, and a running connector keeps its old
  binary until `systemctl restart pco-cloudflared@<tunnel id>`. `pco doctor` is worth a run
  after an upgrade of Proxmox VE or of `cloudflared` ([Operations](../operations.md#upgrades)).
- **A copy of what you decided.** The settings and the manual routes are the part that nothing
  makes again; the [files page](../files.md#what-to-back-up) says what else is worth a copy.

## Guides it uses

- Publish a container, and publish a virtual machine: the tag, the Notes, `pco plan` and
  `pco apply`.
- Manual routes: the NAS.
- Observe, then enforce: reading `pco plan` before the first `pco apply`.
- Adopt existing records: for a name that already has a record at Cloudflare.
- Backup and recovery, and upgrade and rollback.
