# Leaving hand-run tunnels

This page describes how to move hostnames from `cloudflared` processes that someone started
by hand to pco, one name at a time, with the old tunnels serving whatever has not moved yet.

## The situation

Over the years `cloudflared` has been started in several places: in the container `110`, in
the virtual machine `120`, and on the node for a service on another host. Each has a tunnel
of its own, made with `cloudflared tunnel create` or in the dashboard, and the names are
routed to it with `cloudflared tunnel route dns` or by hand. Each has a `config.yml` with the
rules, and a credentials file in somebody's home directory. The services listen on
`localhost`, because the connector is on the same machine. Nobody is sure which tunnel serves
which name, and updating `cloudflared` means logging in to each place. A site that is still
reached through a forwarded port, with an `A` record at the address of the router, is in the
same pile.

pco gives all of it one tunnel for each account, with its rules written from the Notes of the
guests, and one connector on the node, confined by the egress filter.

pco does not take over a tunnel. It makes its own, `pco-<install id>`, and it never touches a
tunnel that is not its own or a DNS record that does not carry its marker. A name that an old
tunnel serves keeps being served by it until you hand it over, with `pco adopt`, which is the
step this page is about.

## What does not move

pco publishes HTTP and HTTPS services of guests, and routes by hostname only.

- A tunnel that carries SSH, RDP, raw TCP or a route to a private network is not something
  pco does. Leave it where it is.
- A rule of `cloudflared` that matches a path has no equivalent in the Notes.
- Of the options of an origin, pco writes `noTLSVerify`, `originServerName` and
  `httpHostHeader`, which the Notes set, and `matchSNItoHost` for an HTTPS wildcard route. It
  writes no other. The table below maps them.
- A service that is not on a guest, as a NAS is not, can be a manual route, within
  `manualCIDRs` ([Security](../security.md#manual-routes)).

An Access application, a cache rule or a WAF rule at Cloudflare belongs to a hostname and
not to a tunnel, so it applies to the name after the move as before it. pco does not see
it either way ([Internal tools behind Cloudflare Access](internal-tools-behind-access.md)).

## The setup

For each name, before and after `pco adopt`: the record at Cloudflare points at another
tunnel, and the connector that serves it is on the node and not in the guest:

~~~mermaid
flowchart LR
    subgraph before["Before"]
        direction TB
        dns1["shop.example.com: CNAME of the old tunnel"] --> old["Tunnel of the hand-run cloudflared"]
        old --> inguest["cloudflared in the guest, to localhost:8443"]
    end
    subgraph after["After pco adopt shop.example.com"]
        direction TB
        dns2["shop.example.com: CNAME of tunnel pco-7f3a9c0d41b2, marked pco:7f3a9c0d41b2"] --> new["Tunnel pco-7f3a9c0d41b2"]
        new --> connector["Connector on the node"]
        connector --> filter["Egress filter"]
        filter --> guest["Guest, 10.0.0.12:8443"]
    end
    before --> after
~~~

## Decisions

### Profile

The profile you would pick for a new install; the move does not depend on it. With the host
profile the connector is on the node and reaches the guests as the node does. What matters
for the move is that the connector is no longer in the guest: a service that listened on
`127.0.0.1` has to listen on the address of the guest, which pco proves. A service that does
not is `unreachable`, with a note that the connection was refused.

### Admission and identity minimum

Leave both at their defaults, `tag` and `port`. You are the one moving the names, and the
guests are on the node that runs pco. A guest on another node can only be proven at
`observed`; see the [company cluster](company-cluster.md) page.

### Tokens

pco needs a Cloudflare API token with the three permissions of
[Cloudflare token](../cloudflare-token.md#permissions). The credentials file of an old tunnel
is not one, and pco has no use for it. Scope the token to the zones that you move names in. pco will not delete an old tunnel, so there is
nothing to grant for that: you remove them yourself, at the end.

### Egress filter

The filter confines the connectors of pco and no other process. A `cloudflared` that you
started on the node runs as a user other than `pco-connector` and is not confined, so stop it
when its names have moved, and not later. The connector that pco starts is confined from its
first start.

### Web interface access

Nothing particular. An admin can request an adoption in the web interface as well as with
`pco adopt`.

## The move, one name at a time

1. **Install pco and give it a token.** [Quickstart](../quickstart.md) has the steps. A new
   install only observes: it changes nothing at Cloudflare until `pco apply`.
2. **Prepare each guest.** Make its service listen on the address of the guest, tag the guest
   and write its Notes with the names its old tunnel serves. The old connector goes on
   running and nothing changes yet. The options of the old rules map as follows:

   | In `config.yml` | In the Notes |
   |---|---|
   | `service: http://localhost:8080` | `-> :8080` |
   | `service: https://localhost:8443` | `-> https://:8443` |
   | `noTLSVerify: true` | `no-tls-verify` |
   | `originServerName: admin.internal.example.com` | `sni=admin.internal.example.com` |
   | `httpHostHeader: app.internal` | `host-header=app.internal` |
   | `hostname: "*.example.com"` | a wildcard route, with a pattern in `allowHosts` that names it |
   | the apex, `example.com` | a route for it, with `example.com` in `allowHosts` |

   For an HTTPS origin pco asks for the TLS name of the public hostname, unless `sni=` or
   `no-tls-verify` says otherwise. An old rule that named no `originServerName` may have
   worked with a certificate of another name; if `pco diagnose` says the verification failed,
   `sni=` with that name, or `no-tls-verify`, is the answer.

   The Notes of `110`, `120` and a virtual machine `121` that is reached through a forwarded
   port today:

   ~~~text
   ```cf-tunnel
   wiki.example.com -> :8080
   ```
   ~~~

   ~~~text
   ```cf-tunnel
   shop.example.com -> https://:8443 no-tls-verify
   ```
   ~~~

   ~~~text
   ```cf-tunnel
   www.example.com -> :80
   ```
   ~~~

3. **Read the plan.** `pco plan` lists each name that a record of someone else holds. Every
   name that an old tunnel serves is there, and so is the one with the `A` record:

       Records of someone else that stand in the way (pco adopt replaces one):
       NAME              ZONE         TYPE   CONTENT
       shop.example.com  example.com  CNAME  01234567-89ab-cdef-0123-456789abcdef.cfargotunnel.com
       wiki.example.com  example.com  CNAME  01234567-89ab-cdef-0123-456789abcdef.cfargotunnel.com
       www.example.com   example.com  A      203.0.113.10

   While they stand in the way pco does not publish the names, and the old tunnels serve
   them, as before. `pco doctor` warns in its `conflicts` check until each is adopted or its
   name is out of the Notes.
4. **Leave observe-only mode.** `pco apply` makes the tunnel of pco, writes its
   configuration and starts the connector. Wait until `pco status` shows the tunnel verified
   and its connector ready, as in the quickstart. No name has moved yet. A name in the Notes
   that has no record at all is published now.
5. **Adopt a name.** `pco adopt shop.example.com` shows the conflict and asks:

       shop.example.com is held by a record of someone else in zone example.com: CNAME 01234567-89ab-cdef-0123-456789abcdef.cfargotunnel.com.
       Adopting replaces it with a record that points at the tunnel.
       Adopt shop.example.com? [y/N] y
       Adoption of shop.example.com requested; it is made by the next run that can.

   The run that can is one in which the tunnel of pco is verified and its connector ready; a
   request that no run could make is dropped after five minutes. Before it changes the record,
   pco keeps a copy of it in `/etc/pve/pco/adopted.jsonl`. A `CNAME`, which is what a record
   of a tunnel is, is changed in place, so the name never lacks a record. An `A` record is
   deleted and made again as a `CNAME`, in two calls. Then check the name from the outside in:

       pco diagnose shop.example.com

   and watch the old connector's log fall quiet.
6. **Do the same for each name, then retire the old tunnel.** When no name points at an old
   tunnel any more, stop its `cloudflared`, and delete the tunnel in the dashboard: Cloudflare
   refuses to delete a tunnel that still has a connection, which is why the connector goes
   first. Remove its credentials file too.

To go back for a name before step 6, put the old record back in the dashboard from the copy in
`adopted.jsonl`; the old connector is still there and serves it at once. The record has no
marker then, so pco leaves it alone, and `pco plan` lists it as in the way again until the name
is out of the Notes.

## What it ends with

The settings are the defaults. A wildcard or an apex that an old tunnel served needs a pattern
in `allowHosts` before pco publishes it, and shows as `rejected` until then
([Settings](../settings.md#hostname-patterns)). A manual route is the way for a service that is
not on a guest, as in the [homelab](homelab.md) page.

After the move the node holds one connector for each account, the Notes of the guests are the
list of what is published, and `pco routes` is the answer to which tunnel serves which name.

## What to watch

- **A name nobody remembered.** An old tunnel may serve names that are in no Notes. Before you
  delete a tunnel, look in the dashboard at which records point at its id. A name that moved
  is gone from that list, and one that is still there stops answering when the tunnel is
  deleted.
- **Wildcards.** A wildcard answers every name below it that has no rule of its own. Move it
  last, after the names that have rules of their own, and adopt it as any other name once
  `allowHosts` names it.
- **Conflicts that stay.** A name that is in the Notes and whose record is still someone
  else's shows in `pco plan` and in `pco doctor` until it is adopted or taken out.
- **The old connector on the node.** It is not confined by the egress filter. Stop it.
- **The token.** Its expiry, as for any install.

## Guides it uses

- Adopt existing records: `pco adopt`, and moving from hand-run `cloudflared`.
- Publish a container, publish a virtual machine, and HTTPS origins.
- Wildcards.
- Manual routes.
- Observe, then enforce.
