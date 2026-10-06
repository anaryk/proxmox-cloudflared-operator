# Internal tools behind Cloudflare Access

This page describes how to publish tools that only staff should reach, with Cloudflare Access
in front of them and pco behind it. The thing to get right is the order: pco does not manage
Access, so the Access application has to exist before pco publishes the hostname.

## The situation

A company runs a few internal tools in guests on Proxmox VE: a Grafana, a wiki, an admin
console. Staff should reach them from anywhere, signed in with the company's identity
provider, without a VPN and without an inbound port at the firewall. Cloudflare Access does
the signing in and the deciding, at Cloudflare's edge. pco publishes the hostnames through a
tunnel.

The two are separate products with separate settings, and they meet at the hostname.

- **Access** holds an application for each hostname, with the policy that says who may get
  in. Every application is deny by default, and Cloudflare warns that a published application
  with none in front of it "will be available to anyone on the Internet." It is made in the
  Zero Trust dashboard, by a person.
- **pco** makes the tunnel configuration and the proxied DNS record of each hostname that
  the Notes of a tagged guest name, and nothing else at Cloudflare.

## What pco does with Access

Nothing. pco does not create, read, change or delete an Access application or policy. In the
code, the Cloudflare client of pco makes calls to verify a token and to list accounts and
zones, calls for the DNS records of a zone, and calls for the tunnels of an account, their
configuration, run token and connections. It has no call for Access, and the three permissions
pco asks of its token, Cloudflare Tunnel Edit, DNS Edit and Zone Read, do not reach Access
either. The token should not be given more
([Cloudflare token](../cloudflare-token.md#what-the-token-is-used-for)). What pco changes at
Cloudflare is its own tunnel, and the records that carry its marker. An Access application
that you made stays as you made it, and so does one that someone else changes.

There are two consequences. pco cannot tell whether a hostname it publishes has an
application, so it cannot warn about one that has not. And the order of the work is yours to
keep.

## The setup

A staff member's request meets Access at the edge. Only a request that Access lets through
goes on into the tunnel, to the connector on the node, which the egress filter confines to
the addresses pco verified. pco writes the tunnel and the DNS records, and the admin writes
the applications and policies:

~~~mermaid
flowchart TB
    staff["Staff member"] --> access{{"Access: an application for the hostname, and an Allow policy that matches?"}}
    access -->|"no"| login["Sign-in page, or refused: the request goes no further"]
    access -->|"yes"| tunnel["Tunnel"]
    subgraph cloudflare["Cloudflare"]
        access
        tunnel
    end
    tunnel --> connector
    subgraph node["Proxmox VE node, host profile"]
        connector["cloudflared connector"] --> filter["Egress filter"]
        daemon["pco daemon"]
        grafana["lxc/130: Grafana, :3000"]
        wiki["qemu/131: wiki, :8080"]
    end
    filter --> grafana
    filter --> wiki
    daemon -. "tunnel configuration, DNS records" .-> tunnel
    admin["Admin, in the Zero Trust dashboard"] -. "applications and policies" .-> access
~~~

## Decisions

### Profile

Either. Access is in front of the tunnel and does not depend on how pco runs. The example is
the host profile, with the guests on the node that runs pco.

### Admission and the hostname list

The risk that is particular to this setup is a hostname published before its application
exists. A guest that is tagged and whose Notes name a hostname is published at the next
cycle, and nothing in pco knows that Access is not ready. So make the hostname list the gate:
set `allowHosts` to exactly the names that have an Access application, and add a name to it
only once its application is in place.

A name that is not in `allowHosts` is not published. The entry for it is dropped, and the
issue `hostname "x" is not allowed by policy` shows it in `pco status`. Teams can write their
Notes ahead of time and nothing goes out until IT adds the name. That works in either
admission mode. Leave `admission` at `tag` when only IT edits guests. Use `approve` when teams
may clone or edit them, as [Security](../security.md#approval-mode) describes. Use exact
names: a pattern such as `*.example.com` allows every name below it, and the wildcard too.

Cloudflare lets the hostname of an application be a wildcard. As Cloudflare
[describes it](https://developers.cloudflare.com/cloudflare-one/access-controls/policies/app-paths/),
`*.example.com` covers `alpha.example.com` and `beta.example.com` and not the apex or a name
two levels down. In a zone that holds nothing but tools, one such application is less work
than one for each name, and `allowHosts` still keeps a mistyped name from being published.
Where the zone also holds public names, a wildcard application would stand in front of them
too, so make one for each tool.

An `allowHosts` that is not empty also keeps every other name from being published by this
install, so give the internal tools an install of their own, a node of their own, or put all
that the install publishes in the list.

### Identity minimum

Leave `identityMinimum` at `port`, with the guests on the node that runs pco. See the
[company cluster](company-cluster.md) page for guests on other nodes.

### Tokens

Two tokens, kept apart on purpose. The token of pco has the three permissions of
[Cloudflare token](../cloudflare-token.md#permissions) and no Access permission. Whoever
makes the Access applications does it with their own login, or with a token of their own for
Access that pco never receives. A token of pco that could also edit Access policies could
open every tool at once.

The zone is a decision too. A hostname such as `grafana.example.com` is one level below the
zone `example.com`, which the universal certificate of Cloudflare covers. A name two levels
down, such as `grafana.tools.example.com`, gets the warning `more than one level below
example.com: needs an advanced certificate`.

### Egress filter

As on any install: on, and unchanged by Access, which is not in the data path on the node.
It still matters here for one reason: a stolen token of pco can point a rule at any address,
and the filter refuses every address that is not a verified target
([Security](../security.md#the-connector-egress-filter)).

### Web interface access

Keep it off the tunnel. The web interface of pco and the interface of Proxmox are on the
node, whose address is on the denylist, so no route in a guest's Notes can publish them; a
manual route with `allowNode` could, and this setup has none. Staff who administer pco use
it on the management network, with the Proxmox account they have, as admins (`Sys.Modify` on
`/`) or readers (`Sys.Audit` on `/`).

## What it ends with

The settings:

~~~json
{
  "allowHosts": ["grafana.example.com", "wiki.example.com", "console.example.com"]
}
~~~

Applied as in [Settings](../settings.md#changing-the-settings). The Notes of the Grafana
container `130`:

~~~text
```cf-tunnel
grafana.example.com -> :3000
```
~~~

and of the console, which speaks HTTPS with a certificate of its own:

~~~text
```cf-tunnel
console.example.com -> https://:8443 no-tls-verify
```
~~~

To add a tool, in this order:

1. In the Zero Trust dashboard, make a self-hosted application for the hostname, with an
   Allow policy for the people who may use it. Cloudflare's
   [guide](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/self-hosted-public-app/)
   has the steps.
2. Add the hostname to `allowHosts`.
3. Tag the guest and write its Notes, if that is not done yet. Once the name is allowed, the
   next cycle publishes it; `pco sync` asks for one now.
4. From a machine outside, with no session, ask for it:

       curl -sI https://grafana.example.com/

   The answer must be Access's, a redirect to the sign-in of your team, or a refusal, and not
   the tool's. If it is the tool's, the application does not cover the hostname: take the name
   out of `allowHosts` at once, and the next cycle takes its rule out of the tunnel.

To take a tool down, in the opposite order: take the name out of the Notes and out of
`allowHosts`, wait for the grace period to remove its record, and then delete the application.

## What to watch

- **The check from outside.** It is the only thing that shows the hostname is protected. Make
  it part of adding a tool, and again when someone edits applications.
- **`pco diagnose` does not test Access.** Its last step is a request that the daemon makes
  straight to the guest, not through Cloudflare, so it passes whatever Access says.
- **A bypass or a broader policy.** A policy that someone adds in the dashboard, a Bypass
  policy or an Allow for everyone, opens the hostname, and pco does not see it. The review of
  Access belongs to whoever owns Access.
- **A route that is not served.** Staff who get through Access reach the tunnel, and a route
  that is held or withdrawn answers 503 there. `pco routes` says why. A guest that is stopped
  is such a case.
- **The guest itself.** Access guards the way in from the internet. A machine on the
  company's network can still reach the guest directly, and what is on it should sign users
  in too. An origin that wants to know who the user is can validate the header
  `Cf-Access-Jwt-Assertion`, which Cloudflare
  [describes](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/authorization-cookie/validating-json/).
- **Names that left.** An application whose hostname pco no longer publishes does no harm and
  is worth deleting.
- **The token.** Its expiry, as for any install.

## Guides it uses

- Publish a container, and publish a virtual machine.
- HTTPS origins: the console above.
- Who may publish: the hostname policy and the admission modes.
- Observe, then enforce.
- Monitoring.
