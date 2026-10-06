# Company cluster

This page describes pco on a Proxmox VE cluster that several teams use and one team
administers: where to install it, what to do about the guests of the other nodes, how to
keep the people who edit guests from publishing what they like, and what stops when the node
that runs pco does.

## The situation

A company runs a cluster of three nodes, `pve1` to `pve3`, with shared storage. Teams run
their applications in guests. IT administers the cluster and holds `Sys.Modify` on `/`; the
teams hold `VM.Config.Options` on their own guests, which lets them edit the Notes, and some
of them may clone guests. A customer portal, an API and a documentation site should be
reachable from the internet, and the firewall should keep no inbound port open for them.

Two facts about pco decide the rest.

- **pco runs on one node.** `pco setup` on a second node refuses, and a daemon holds while
  the node registry names another node. The connectors run on that node too.
- **It does not fail over.** If that node is down, every published hostname is down with it,
  and nothing takes over. Moving pco to another node is something an admin does, with the
  steps of the backup and recovery guide; automatic failover is planned for a later release.

## The setup

pco and its connector are on `pve1`. The guests published from `pve1` are proven at `port`.
A guest on `pve2` is listed and its Notes are read, but this node cannot see where the bridge
of `pve2` sends the guest's frames, so it is proven at `observed` at most, and is held back at
the default minimum. The store is on the cluster filesystem and is the same on every node:

~~~mermaid
flowchart TB
    visitor["Visitor"] --> edge["Cloudflare edge"]
    edge -->|"through the tunnel the connector opened"| connector
    subgraph cluster["Proxmox VE cluster"]
        subgraph pve1["pve1: pco runs here, host profile"]
            connector["cloudflared connector"] --> filter["Egress filter"]
            daemon["pco daemon"]
            portal["qemu/131: portal"]
            api["lxc/132: API"]
        end
        subgraph pve2["pve2"]
            reports["qemu/141: reports, proven at observed"]
        end
        subgraph pve3["pve3"]
            other["Guests that are not published"]
        end
        store[("Store on the cluster filesystem, replicated to every node")]
    end
    filter --> portal
    filter --> api
    daemon -->|"ARP only, no forwarding table"| reports
    daemon --> store
~~~

## Decisions

### Profile

The host profile, on `pve1`. The published guests live on the node that runs pco, where the
host profile can prove their addresses at `port`. A company that would rather keep a
daemon off its hypervisors can run the appliance instead; every guest is then proven at
`observed`, which [Hosting tenants](hosting-tenants.md) describes, with what that asks of the
admins.

### Where to install it, and the guests of the other nodes

Install it on the node that holds the guests to publish, and keep those guests there. A
published guest that Proxmox moves to another node, by a migration or by HA, can be proven at
`observed` there at best, so at the default minimum it is held back with the note
`identity level observed is below the required port`, and its hostname answers 503 until it
is back. Plan the moves of these guests as outages of their hostnames.

A guest that has to stay on another node can be served, at a price. The minimum is a setting
of the whole install:

1. Set `identityMinimum` to `observed`. Guests of this node are still proven at `port` where
   they can be; what changes is that routes proven at `observed` are served.
2. Acknowledge each segment, the bridge and VLAN of the guest's card, once an admin has
   decided that routes proven on it may be served: `pco segment list` shows them and
   `pco segment acknowledge vmbr0` serves them. A route at `observed` is held until its
   segment is acknowledged.
3. Expect approvals. Where anyone but the admins holds `VM.Config.Network` on the guest, or
   the MAC that answers for its address is not the one first seen, the route waits for
   `pco guest approve`.

[Identity](../identity.md#identityminimum) says what the lower level proves and does not, and
the table in [Security](../security.md#identity-levels-against-five-attackers) which attacker
each level stops. At `observed`, whoever can set the MAC of a guest's card and write its
Notes can reach any device on the segment, which is the reason for steps 2 and 3 and the
reason to prefer moving the guest.

### Admission

Set `admission` to `approve`. Several people can edit Notes, and some can clone a guest:
Proxmox copies the tags of the source to a clone, so whoever may clone a tagged guest gets a
tagged guest of their own and fills its Notes. In mode `approve` a guest is published only
after an admin has looked at it and run `pco guest approve`; a clone has an identity of its
own and waits ([Security](../security.md#approval-mode)).

The tag is the other half. `pco setup` registers `cf-tunnel` as a registered tag, so that
only a user with `Sys.Modify` on `/` can set it. IT decides which guests may publish, by
tagging them; the teams write what they publish; IT approves the guest. An approval is of a
guest, not of its hostnames, so a team can add a name to the Notes of an approved guest later.
The hostname policy is what bounds that:

- `denyHosts` for the names that no team may publish, such as the admin console and
  everything below an internal prefix.
- `maxHostnamesPerGuest` below its default of 32; a guest that names more publishes none.
- The Cloudflare token, below: a name in a zone that no token serves is never published.

A new install starts in observe-only mode, so set the mode before `pco apply`. On an install
that is publishing, approve the published guests first. `pco guest approve` works in mode
`tag` too. Changing the mode takes every route of a guest that is not approved off the air at
the next cycle.

### Tokens

One token for the zones the company publishes in, and nothing else: the three permissions of
[Cloudflare token](../cloudflare-token.md#permissions), on the account that holds those
zones and on those zones only. A zone that the token does not see cannot be published in, so
the zones of the company's other domains are out of reach of anyone who edits Notes. With
zones in two accounts, store one token for each; pco makes one tunnel in each account.

The token is stored in `/etc/pve/priv/pco`, which the cluster filesystem replicates to every
node. Every user who is root on any node can read it, and so can whoever holds a backup of
`/etc/pve`. Treat the backups of the cluster filesystem as secrets, and scope the token for
that reason.

Give it a time to live and put the replacement in a calendar. `pco status` and `pco doctor`
warn 30 days before it ends, and [the rotation](../cloudflare-token.md#rotating-a-token)
takes four steps. pco leaves 200 of the 1200 requests in 5 minutes that Cloudflare allows a
user to other tools, and keeps to what Cloudflare says is left. If automation of the company,
Terraform say, calls the API as the same user and spends more than that, lower
`cloudflareBudget` from its default of 1000 and restart the daemon, since that setting is
read at start ([Settings](../settings.md#cloudflare)).

### Egress filter

On, as it is after setup. It is what stands between a stolen token and the management
network: a rule written at Cloudflare for `10.0.0.1:8006` gets a refused connection. If some
address on a network that the node reaches must never be a target, whatever a Notes block
or Cloudflare says, `pco egress block <address>` makes sure of it; it is local to the node
and needs no daemon. If configuration management of the company loads an nftables ruleset
that begins with `flush ruleset`, the table of pco is removed each time it runs and the daemon
puts it back; `pco doctor` has a check for `nftables.service`, and the event log says each
time the table was changed ([Security](../security.md#keeping-the-table-in-place)).

### Web interface access

Admins of pco are the users that hold `Sys.Modify` on `/`, readers the users that hold
`Sys.Audit` on `/`; they sign in with what Proxmox already knows of them. Give the helpdesk a
role with `Sys.Audit`, such as PVEAuditor, and nothing more: a reader can read the state of
the install and cannot change anything.

The interface listens on the address of `pve1` on port 8643. Restrict that port with the
Proxmox firewall to the admin network, as you would port 8006. If staff reach it under a
name of their own, put the name in `PCO_WEB_HOSTS` in `/etc/default/pco-web` and restart
`pco-web`, and give it a certificate that the company's own CA signed:

    pco setup --repair --web-cert own --web-cert-file /root/pco.example.com.crt --web-key-file /root/pco.example.com.key

## What it ends with

The settings that differ from the defaults:

~~~json
{
  "admission": "approve",
  "denyHosts": ["admin.example.com", "*.internal.example.com"],
  "maxHostnamesPerGuest": 4
}
~~~

Apply them as in [Settings](../settings.md#changing-the-settings). The Notes of the guest
`131`, written by the team that owns it:

~~~text
```cf-tunnel
portal.example.com -> :8080
api.example.com    -> :8081
```
~~~

IT sets the tag, as a user with `Sys.Modify` on `/`, and approves:

    qm set 131 --tags 'cf-tunnel'
    pco guest list
    pco guest approve qemu/131

`pco guest list` shows who is approved, who changed since, and who waits, with the node each
runs on:

    Approved guests:
      GUEST             IDENTITY  NOW
      qemu/101 (web-1)  uuid:101  the same
      qemu/102 (db-1)   uuid:old  changed to uuid:102: approve it again to publish it
      lxc/300           uuid:300  not in the last listing

    Waiting for approval (pco guest approve <owner>):
      qemu/103 (new-1)
      qemu/104

    Tagged guests:
      GUEST             NODE  RUNNING  IDENTITY  APPROVAL  ROUTES  ISSUES
      qemu/101 (web-1)  pve1  yes      uuid:101  approved  2       0
      qemu/102 (db-1)   pve1  no       uuid:102  changed   0       1
      qemu/103 (new-1)  pve2  yes      uuid:103  waiting   0       0

With the portal on `pve1` and a report server on `pve2`, `pco routes` shows what the default
minimum does to the second:

    HOSTNAME             STATE        LEVEL     SERVICE                OWNER     ZONE         NOTE
    portal.example.com   active       port      http://10.0.0.31:8080  qemu/131  example.com  -
    reports.example.com  unreachable  observed  -                      qemu/141  example.com  identity level observed is below the required port

## What to watch

- **The node.** If `pve1` is down, everything published is down. Write down how pco is moved
  to another node before the day it is needed, and test it on a node that is not in use. The
  state that cannot be made again is in `/etc/pve/pco` and `/etc/pve/priv/pco`, which every
  node has, and in the manifest in `/var/lib/pco` of `pve1`
  ([Files](../files.md#what-to-back-up)).
- **Quorum.** Without quorum the cluster filesystem is read-only. Published routes keep
  serving. An approval or a change of the settings fails, and a cycle that has a claim to
  save holds until it can ([Architecture](../architecture.md#one-writer)).
- **Reboots of `pve1`.** A Proxmox upgrade that reboots the node is an outage of all
  hostnames: after the boot they answer 502 until the daemon's first cycle has verified the
  routes. Migrating the guests away first does not avoid it: they are held back while they are
  on another node.
- **Guests that wait.** `pco status` has the line `Approval:` with how many wait, and
  `pco doctor` warns for each. A team that clones a guest to test something will ask why it
  is not served.
- **Held routes.** `pco status` says how many routes the minimum and the rules at `observed`
  hold, and why. A line that counts guests of other nodes means someone moved one.
- **The token.** Its expiry, and who else spends the Cloudflare API of the same user.
- **Monitoring.** `pco status` exits 1 when there is a problem, and `pco status --json` has
  the fields to alert on; `pco doctor` can run from a timer ([Operations](../operations.md#exit-codes-and---json)).

## Guides it uses

- Clusters: which node to install on, the guests of other nodes and segments, what stops
  when that node is down.
- Who may publish: the gate tag, the admission modes, approvals.
- Several tokens and accounts, and observe, then enforce.
- Backup and recovery: what to back up, and moving pco to another node.
- Upgrade and rollback, and monitoring.
