# Hosting tenants

This page describes pco as the appliance, in a container, for a hosting company whose
customers publish services from their own machines: what the appliance can prove about those
machines, what the company has to hold to itself, and how a customer is taken on and off.

## The situation

A small hosting company rents virtual machines on a Proxmox VE node. Each customer, a tenant,
has a machine or two on a VLAN of its own, and a Proxmox account that may manage those
machines and nothing else. A tenant wants a web service of its machine on the internet under a
name of the company's zone, `acme.example.net`. The company will not open inbound ports for
it, give the tenant a Cloudflare account or install anything on its hypervisors beyond
Proxmox, and does not trust a tenant's machine or account with more than its own.

The appliance fits that. pco and its connectors run in an unprivileged container, in the
pool `pco`, with the state on a volume of its own that backups leave out and a Proxmox API
token of its own, privilege-separated. The node holds the container and nothing else of
pco. The cost is the one in [Profiles](../profiles.md#side-by-side): the container cannot
read the forwarding table of a bridge, so the highest level a guest can be proven at is
`observed`, and the rules for that level decide who may publish.

## The setup

A tenant's hostname reaches its machine through Cloudflare's edge and a connector inside the
appliance. The appliance reaches each tenant network through a card of its own on it, and
its web interface listens on the card on the management network, where tenants are not:

~~~mermaid
flowchart TB
    visitor["Visitor"] --> edge["Cloudflare edge"]
    staff["Company staff"]
    subgraph node["Proxmox VE node, nothing installed but Proxmox"]
        subgraph appliance["lxc/120: the pco appliance, unprivileged, pool pco"]
            connector["cloudflared connectors"] --> filter["Egress filter"]
            daemon["pco daemon"]
            web["Web interface, on net0 only"]
        end
        subgraph acmenet["VLAN 20: tenant acme"]
            acme["qemu/151: web, 10.0.20.51:8080"]
        end
        subgraph globexnet["VLAN 30: tenant globex"]
            globex["qemu/161: web, 10.0.30.61:8080"]
        end
        pveapi["Proxmox API"]
    end
    staff -->|"HTTPS, port 8643"| web
    edge -->|"through the tunnel the connector opened"| connector
    filter -->|"card net1"| acme
    filter -->|"card net2"| globex
    daemon -->|"scoped token"| pveapi
~~~

## Decisions

### Profile

The appliance, for the reason in the situation. What it asks of the company is to take the
levels seriously: at `observed` the proof is that only the guest answers ARP for the address,
and that the card of the guest has the MAC it had the first time. Nothing proves that the
frames of the guest go through its own port. [Identity](../identity.md) and the
[table in Security](../security.md#identity-levels-against-five-attackers) say what that
does not stop. The install makes the container, the pool, the token and the settings; the
appliance page has its steps.

The appliance runs on the one node it was installed on, and nothing moves it to another
when that node fails. A copy of the container, such as a clone, serves nothing: the container proves in every
cycle that it is the one that was installed.

### Identity minimum and segments

The installer sets `identityMinimum` to `observed`, since the appliance has no higher level
to ask for. Two rules of that level then decide what is served, and both are the company's to
apply. A segment, the bridge and VLAN of a guest's card, is served only after an admin
acknowledged it. And a guest waits for an approval of its own when anyone but an admin holds
`VM.Config.Network` on it, which a tenant does on its own machine as a rule, and when the MAC
that answers for its address is not the one first seen
([Identity](../identity.md#routes-that-wait-for-an-admin)).

Give each tenant a VLAN of its own, and put nothing on it that is not the tenant's. A route
can only be aimed at an address the guest itself answers for, but a tenant who may set the
MAC of its card can aim at any device on its segment, so the segment of a tenant should hold
the tenant's machines and a gateway, and nothing else of the company's.

The appliance needs a card of its own on each of these networks, with an address in the
network. pco does not attach cards in this release, so they are added to the container in
Proxmox, as to any container. For the two networks of the diagram:

    pct set 120 --net1 name=eth1,bridge=vmbr1,tag=20,ip=10.0.20.2/24
    pct set 120 --net2 name=eth2,bridge=vmbr1,tag=30,ip=10.0.30.2/24

Its segment is then the one the card is on, `vmbr1:20` for the first, and each segment is
acknowledged once:

    pco segment list
    pco segment acknowledge vmbr1:20

### Admission, the tag, and who writes the names

Set `admission` to `approve`. The tags of pco are registered, so a tenant cannot tag its own
machine: the company tags the machine when the tenant is taken on, as a user with
`Sys.Modify` on `/`. The tenant then writes the hostnames in the Notes of the machine, which
it may edit, and an admin approves the machine. A tenant that clones its tagged machine gets a
machine with another identity, which waits.

An approval is of a guest and not of its hostnames, so what bounds a tenant's later edits is
the hostname policy, and it is one list for the whole install. There is no policy per
tenant. Two choices follow:

- **The tenant writes the names.** Put the names the company has sold in `allowHosts`, as
  exact names, and set `maxHostnamesPerGuest` low. Do not write a pattern such as
  `*.example.net`: a pattern `*.x` also lets a guest publish the wildcard `*.x`, which
  answers every name of the zone that has no rule of its own. Two tenants that name one
  allowed hostname get the first holder and a conflict for the second, so add a name to
  `allowHosts` when it is sold, and tell its tenant. The list cannot say that `acme` may
  publish `acme.example.net` and `globex` may not.
- **The company writes the names.** Leave the tenants' machines without the gate tag. Their
  Notes are then never read, whatever they say. The company makes each route itself, as a
  manual route to the guest, which needs no tag:

      pco route manual add acme.example.net --guest qemu/151 --port 8080 --via 10.0.20.51

  The route is the company's own, so the policy does not apply to it, and the rules at
  `observed` do: the segment, the MAC and the approval of the guest. A machine without the
  tag is not watched: pco rereads its configuration every five minutes and does not ask its
  guest agent for addresses, so the route names the address with `--via 10.0.20.51`, or the
  machine has a static address in its Proxmox configuration.

Each name should be one level below the zone, as `acme.example.net` is: the universal
certificate of Cloudflare covers one level only, and a deeper name gets the warning
`more than one level below example.net: needs an advanced certificate`.

### Tokens

The Cloudflare token is for the zone `example.net` and its account and nothing else, with the
three permissions of [Cloudflare token](../cloudflare-token.md#permissions). It is stored in
the state volume of the appliance, which backups of the container leave out; `pco doctor` fails
its `volume` check if that stops being so. The Proxmox token of the appliance is made by the
installer and holds what the role `PCO` holds, which is read-only, on `/`.

The privileges of the tenants matter as much. A principal that is not an admin and holds on
the appliance any of `VM.Console`, `VM.Config.*`, `VM.Clone`, `VM.Backup`, `VM.Snapshot*`,
`VM.Migrate`, `VM.PowerMgmt`, `VM.Allocate` or `Permissions.Modify`, whether it is granted
on `/vms/120`, on a path above it or through the pool `pco`, can reach into the container
and so to the tokens in it. While there is one, pco serves nothing and its connectors are
stopped. Give each tenant its roles on its own machines, `/vms/151`, or on a pool of its own,
and never on `/`, `/vms` or the pool `pco`.
The installer refuses while such a principal exists, unless `--deny-access` adds a `NoAccess`
line for each, and the `access` check of `pco doctor` fails while one exists.

### Egress filter

The filter of the appliance is a table of the container, with the same rules as on a host:
a connector reaches Cloudflare's edge, the resolvers of the container, DNS over TLS at
1.1.1.1 and 1.0.0.1, and the verified targets, and nothing else. A rule at Cloudflare for the
address of the node or of another tenant's machine gets a refused connection. Nothing for the
company to set.

### Web interface access

The appliance has a sign-in of its own, which asks Proxmox for a ticket: a user, a realm and
a password, and a TOTP or recovery code when the account has a second factor. A security key
does not work there. `root@pam` is refused unless `PCO_WEB_ALLOW_ROOT=1` is set, which the
company should not do; use accounts of its own staff. Admins are the accounts with
`Sys.Modify` on `/`, readers those with `Sys.Audit` on `/`, so a tenant must not hold
`Sys.Audit` on `/`: a reader can read the state of the whole install, which names every
tenant's hostnames.

It listens on the address of the card `net0` only, on port 8643, and refuses to start on any
other, so that the tenants, who are on the other cards, cannot reach the sign-in page.
`net0` belongs on the management network and not on a tenant's. `pco doctor` warns if
`PCO_WEB_LISTEN` names another address, and, in its `segment access` check, if a principal
other than an admin may use `SDN.Use` on that network.

## What it ends with

The settings the company sets, as in [Settings](../settings.md#changing-the-settings) (the
installer has made `identityMinimum` `observed` already):

~~~json
{
  "admission": "approve",
  "allowHosts": ["acme.example.net", "globex.example.net"],
  "maxHostnamesPerGuest": 2
}
~~~

The Notes of the tenant's machine `151`:

~~~text
```cf-tunnel
acme.example.net -> :8080
```
~~~

To take a tenant on: add its cards and its segment; tag the machine
(`qm set 151 --tags 'cf-tunnel'`); add the name to `allowHosts`; let the tenant write its
Notes. The commands below run in the appliance (`pct enter 120` opens a shell in it). The web
interface can approve and revoke guests and make manual routes, and a segment is acknowledged
with the command only. At first the route is held at the segment:

    HOSTNAME            STATE        LEVEL     SERVICE  OWNER     ZONE         NOTE
    acme.example.net    unreachable  observed  -        qemu/151  example.net  segment vmbr1 VLAN 20 is not acknowledged; pco segment acknowledge vmbr1:20
    globex.example.net  unreachable  observed  -        qemu/161  example.net  segment vmbr1 VLAN 30 is not acknowledged; pco segment acknowledge vmbr1:30

`pco segment list` has both segments, unacknowledged:

    SEGMENT   ACKNOWLEDGED  ROUTES
    vmbr1:20  no            1
    vmbr1:30  no            1

Acknowledging asks first, and says what it serves:

    1 route at observed waits for segment vmbr1 VLAN 20:
      acme.example.net  qemu/151 (acme-web)
    Acknowledging it serves the routes at observed proven on it, unless they wait for an approval of their guest.
    Acknowledge segment vmbr1 VLAN 20? [y/N] y
    Acknowledged segment vmbr1 VLAN 20; its routes at observed are served from the next cycle.

Then the route waits for the approval of its guest, as its tenant holds `VM.Config.Network`
on it:

    HOSTNAME            STATE        LEVEL     SERVICE  OWNER     ZONE         NOTE
    acme.example.net    unreachable  observed  -        qemu/151  example.net  delegated: acme-admin@pve holds VM.Config.Network; pco guest approve qemu/151
    globex.example.net  unreachable  observed  -        qemu/161  example.net  segment vmbr1 VLAN 30 is not acknowledged; pco segment acknowledge vmbr1:30

`pco guest approve qemu/151` shows what it records before it records it:

    qemu/151 (acme-web) waits for approval in identity uuid:0123abcd-0000-4000-8000-000000000151; approved, it publishes acme.example.net.
    It waits because:
      delegated: acme-admin@pve holds VM.Config.Network
    Approving it records MAC bc:24:11:00:00:51.
    Approved qemu/151 in identity uuid:0123abcd-0000-4000-8000-000000000151.
    From the next cycle its routes no longer wait for an approval.

When both tenants are through, `pco routes` shows them at the level they are proven at:

    HOSTNAME            STATE   LEVEL     SERVICE                 OWNER     ZONE         NOTE
    acme.example.net    active  observed  http://10.0.20.51:8080  qemu/151  example.net  -
    globex.example.net  active  observed  http://10.0.30.61:8080  qemu/161  example.net  -

To take a tenant off, take the tag off the machine and the name out of `allowHosts`,
`pco guest revoke qemu/151`, `pco segment revoke vmbr1:20`, and the card out of the
appliance. The record is removed after the grace period.

## What to watch

- **A tenant that changes its card.** Another MAC on the card puts the route on hold again,
  with the reason `MAC changed from <MAC> to <MAC>; approve the guest to accept it`. Look at
  whose MAC the new one is before approving.
- **Waiting guests.** The `Approval:` line of `pco status`, and `pco guest list`, say which
  machines wait. A tenant that cloned its machine, or restored it with new addresses, will ask.
- **The privileges of tenants.** The `access` check of `pco doctor` is the one that says a
  principal can reach into the appliance, and the connectors stop until it is put right. Look
  at it after anyone changes roles in Proxmox.
- **Copies.** A snapshot, a replication job or a clone of the container is a copy of the
  tokens. `pco doctor` warns for a snapshot and fails for a copy. The state volume is kept out
  of backups on purpose, as a backup would carry the credentials and the keys, and the
  `volume` check fails if that changes. After a restore or a rollback the commands are
  `pco appliance repair` and `pco appliance recover`; the appliance page describes them.
- **The node.** The appliance is on one node. While it is down, so is everything published.
- **Names.** `pco claims list` says who holds each hostname and who waits for it, and
  `pco events` records what the admins did.

## Guides it uses

- Who may publish: the gate tag, the admission modes, approvals.
- Manual routes: the names the company writes itself.
- Several tokens and accounts, and observe, then enforce.
- The appliance guides: install, upgrade, and recovery of the appliance itself.
- Monitoring.
