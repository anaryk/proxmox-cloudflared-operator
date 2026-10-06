# FAQ

Short answers to the questions that come up first, each with the page that has the whole
answer. The words pco uses in a sense of its own are in the [Glossary](glossary.md).

## Does pco need root?

Yes, on the node. The commands that set up, change or take down the node work on it directly,
without the daemon, and need root: on a host `pco setup`, `pco uninstall`, `pco egress` and
`pco web cert`, and for the appliance `pco appliance install` on the node and `pco upgrade`
inside it. The commands that show or change the state, such as `pco status`, `pco plan` and
`pco apply`, ask the daemon through its socket, `/run/pco/pco.sock`, which answers root and
the user of the web interface, `pco-web`, and no one else. Run by another user, they print
`permission denied on /run/pco/pco.sock: run as root` and exit with status 2. Only
`pco version` and `pco completion` need neither root nor the daemon.

The daemon, `pco.service`, runs as root too. It opens a raw socket to ask ARP, reads the
forwarding tables of the bridges, loads the nftables table, runs `systemctl` for the
connectors and reads the tokens from `/etc/pve/priv/pco`. Treat it as a root-equivalent part
of the node. What keeps the rest small: the Proxmox token it uses can only read, the
connectors run as the user `pco-connector` with no capabilities and under the egress filter,
and the web interface runs as `pco-web` in a systemd sandbox.
[Security](security.md#what-runs-with-which-rights) has the whole list.

If you do not want a root daemon on the hypervisor, the appliance profile runs pco in an
unprivileged container and installs nothing on the node. [Profiles](profiles.md) sets the two
side by side.

## What does pco change on the node and in Proxmox?

The package installs `/usr/bin/pco`, five systemd units in `/usr/lib/systemd/system`, the man
pages, the completion scripts and these pages, the release key and the list of vetted
`cloudflared` versions in `/usr/share/pco`, and a `sysusers.d` file for the users
`pco-connector` and `pco-web`. The units are `pco.service`, the daemon; `pco-egress.service`,
which loads the egress filter; `pco-cloudflared@.service`, the connector of one tunnel;
`pco-web.service`, the web interface; and `pco-net.service`, which only the appliance uses and
nothing starts on a host. The package depends on `nftables` and enables nothing.

`pco setup` does the rest. It looks at each step before it takes it, so running it again
changes nothing that is in order:

- In Proxmox it makes the role `PCO`, which can only read, the user `pco@pve` with the API
  token `pco@pve!pco`, and grants the role to the user on `/`. It registers the tags
  `cf-tunnel` and `cf-tunnel-managed` as registered tags, so that only a user with
  `Sys.Modify` on `/` can set them.
- On the node it makes the store in `/etc/pve/pco`, `/etc/pve/priv/pco` and `/var/lib/pco`,
  and enables and starts `pco-egress.service` and `pco.service`. When `cloudflared` is missing
  it installs it from Cloudflare's package repository (it asks), which adds the apt source
  `/etc/apt/sources.list.d/cloudflared.sources` and the key
  `/usr/share/keyrings/cloudflare-main.gpg`.
- Unless `--no-web` is given, it also sets up the web interface: it writes
  `/etc/default/pco-web` with the address to listen on, by default the node's own address on
  port 8643, and `/etc/pco/web/` with the certificate and key that it serves. By default that
  is a key of its own and a certificate of 90 days signed by the cluster CA, which the daemon
  renews. It enables and starts `pco-web.service`.

From then on the daemon starts a connector unit, `pco-cloudflared@<tunnel id>.service`, for
each tunnel, and loads the nftables table `inet pco_egress`. That table is pco's own and looks
at the packets of the connectors only; other tables and the Proxmox firewall are not touched.

Everything setup creates is listed in `/var/lib/pco/manifest.json`, and `pco uninstall`
removes what it lists and nothing else. pco never changes a guest, its tags or its Notes.
[Files](files.md) lists every path, and
[Architecture](architecture.md#what-pco-never-touches) what pco never touches.

## What happens when Cloudflare is down?

It depends on what is down.

If the edge is down, published hostnames are down, and pco can do nothing about it: it is not
on the path of a request. The connector is `cloudflared` under systemd, which starts it again
when it exits, and it connects again when the edge answers.

If only the API does not answer, nothing about a request changes, because a request never
needs the API. What stops is every change. pco does not take a failed call for an empty
answer: it removes no record, tunnel or connector because Cloudflare did not say what exists,
and it tries again in the next cycle, every 10 seconds by default. A listing of the
zones that fails falls back on the earlier one. A daemon that starts while the API is out of
reach cannot list the zones yet, and holds its cycles for every account until it can; the
connectors keep running meanwhile. `pco status` says why in its problems. A check of a token
that gets no answer says `Cloudflare did not answer` and is no verdict on the token.
See [Holding back](operations.md#holding-back) and
[Cloudflare did not answer](troubleshooting.md#cloudflare-did-not-answer).

## What happens when the daemon stops?

Published hostnames keep answering. The connectors are units of their own, and they run on
when the daemon stops, restarts or is upgraded; the egress table stays in the kernel as it
was. What stops is every decision: new routes, withdrawals and the removal of records.
[Operations](operations.md#the-daemon-and-its-units) has the details.

## Can a guest publish itself?

No. A guest publishes through its tag and its Notes. Both are part of its configuration in
Proxmox, which a process in the guest cannot write unless it holds Proxmox credentials.
Someone with rights in Proxmox has to put the gate tag on the guest, which takes `Sys.Modify`
on `/` while the tag is registered, as `pco setup` arranges, and to write the routes into its
Notes, which takes `VM.Config.Options` on the guest.

Even then the Notes only ask:

- The hostname policy applies. An apex or a wildcard is published only when an `allowHosts`
  pattern names it, `denyHosts` refuses names, and a guest whose Notes name more than
  `maxHostnamesPerGuest` hostnames, 32 by default, publishes none.
- The first guest to claim a hostname keeps it. A clone carries the tag and the Notes of its
  original, asks for the same hostnames and does not get them while the original holds them.
- The address has to be proven to belong to the guest before it is served, so naming the
  address of the node or of another machine does not publish it. [Identity](identity.md) says
  how, and [Security](security.md#identity-levels-against-five-attackers) what each level
  stops and where it stops.

The admission mode `approve` asks an admin to approve each guest with `pco guest approve`
before its routes are served. Use it when people other than the admins can edit or clone
guests; see [Approval mode](security.md#approval-mode).

## What does a tenant see?

A tenant here is a Proxmox user who has rights on some guests and none on the node itself.
Such a user cannot sign in to the web interface: signing in takes `Sys.Audit` on `/`, and the
sign-in is refused without it.

In Proxmox nothing changes for them: pco only reads, and writes no guest, tag or Notes. What
they can publish is what the answer above says: nothing, unless an admin tagged their guest,
and then only what the hostname policy and the claims allow.

A user who holds `Sys.Audit` on `/` signs in as a reader, and one who holds `Sys.Modify` on
`/` as an admin. A reader sees the guests they hold `VM.Audit` on: their routes, issues,
events and traffic, the claims and approvals that name them, and the list of guests. The
manual routes, which belong to no guest, are everyone's. A reader changes nothing and does
not see what waits for an admin's confirmation.

What is not tied to a guest, a reader sees whole: the tunnels and connectors, the
credentials (never a token), the zones, the settings, the egress filter, and the problem
lines, which are free text and may name a guest the reader cannot see. So giving a tenant
`Sys.Audit` on `/`, to let them see their own routes, shows them how the node is set up as
well.

## Does pco support IPv6?

Not for the origins. The address of a route is an IPv4 address, and the IPv6 addresses of a
guest are never candidates. A route that names one is rejected:

    invalid target "[::1]:80": expected [http|https://][ipv4]:port

So a guest with only an IPv6 address cannot be published. For any guest, the node also needs
an IPv4 address on the bridge that the guest's network card is attached to, to reach it.

Two other places have an address family. The connectors reach Cloudflare's edge over IPv4 or
IPv6, as `cloudflared` chooses (`--edge-ip-version auto`), and the egress filter lets port
7844 through to public addresses of both. How a visitor reaches the edge is up to Cloudflare
and the settings of your zone: pco changes no zone setting.

## Does pco do Cloudflare Access?

No. pco does not create, change or read Access applications or policies, and the token it
needs has no permission for them. A published hostname is public, and what protects the
application behind it is the application, or what you set up at Cloudflare for that hostname
yourself. pco leaves such a setting alone: at Cloudflare it writes the configuration of its
own tunnels and the records that carry its marker, and nothing else. See
[What there is not](annotations.md#what-there-is-not) and
[What Cloudflare sees](security.md#what-cloudflare-sees).

## What does it cost?

pco is free software under the MIT licence. It adds no cost beyond the node it runs on: the
daemon, a `cloudflared` connector for each tunnel, and one tunnel for each Cloudflare account
that holds a zone with routes. What Cloudflare charges for the account, the zone and the
tunnels is for Cloudflare to say, and it changes, so read its current terms.

The one cost pco causes at Cloudflare is API requests, which it keeps inside Cloudflare's
limit of 1200 in five minutes: at most `cloudflareBudget` of them for each credential, 1000
by default. A cycle that has nothing to change takes three requests for one account and one
zone. See
[What Cloudflare's rate limit costs](operations.md#what-cloudflares-rate-limit-costs).

## Does pco work on a cluster, and does it fail over?

pco runs on one node. `pco setup` refuses on a second node of a cluster, and a daemon holds
when the node registry names another node. On a cluster, install it on the node whose guests
you want to publish. The guests of the other nodes are listed and their routes are read, but
this node cannot see the forwarding tables of their bridges, so they are proven at `observed`
at best, and are served only once `identityMinimum` is lowered to `observed` and their
segment is acknowledged; see [Identity](identity.md#identityminimum).

There is no failover. The connectors run on that node, so when it is down every published
hostname is down with it, and nothing moves pco to another node. That is planned for a later
release; see [pco on a cluster](architecture.md#pco-on-a-cluster).

## Can pco use a tunnel or DNS records that I already have?

Not a tunnel. pco makes its own, `pco-<install id>`, in each account that holds a zone with
routes, and never touches a tunnel it did not make.

A DNS record, in part. A record that pco did not make and that holds a hostname pco is asked
to publish stays as it is, and the route waits. `pco plan` lists the record, and
`pco adopt <name>` replaces it after keeping a copy in `/etc/pve/pco/adopted.jsonl`. See
[Records in the way, and `pco adopt`](troubleshooting.md#records-in-the-way-and-pco-adopt).

## How do I uninstall pco?

As root on the node, in this order:

    pco uninstall
    apt purge pco

`pco uninstall` lists what it will remove and asks once. It also asks whether to delete the
DNS records and the tunnel that the install has at Cloudflare, and whether to remove
`cloudflared` when setup installed it. A script answers with flags:

    pco uninstall --yes --purge-cloudflare --remove-cloudflared

`--keep-cloudflare` leaves the records and the tunnel instead. The command removes what the
manifest lists and nothing else: guests, their tags and Notes, and records without the marker
of the install stay. Run it before you remove the package, since a purge first loses the
manifest and leaves the connectors running. [Uninstall](uninstall.md) has the order and what
is kept, and [pco appliance uninstall](cli/pco-appliance-uninstall.md) takes the appliance
off a node.

## How is pco different from running cloudflared by hand?

pco runs `cloudflared` too: the same connector, the same Cloudflare Tunnel, the same edge. It
is not in the data path. What it takes over is the bookkeeping around them.

By hand you make the tunnel, run the connector, write an ingress rule with the address of a
guest for each hostname, make the DNS record, and keep the three in line when a guest is
added, renumbered, cloned or removed. Nothing checks that the address you wrote is still the
guest's, and the connector can reach whatever the node can.

With pco the Notes of a guest are the one place that says what is published, and pco:

- writes the tunnel configuration and the records, and removes them once the Notes stop
  asking for them and the grace period has passed;
- proves that an address belongs to the guest, and withdraws the route when the proof is lost;
- runs each connector as a confined unit, and lets it reach Cloudflare, the resolvers of the
  node and the verified addresses, and nothing else, the Proxmox web interface included;
- settles who holds a hostname when two guests ask for it, and says why a hostname does not
  answer in `pco status`, `pco diagnose` and `pco doctor`.

What you give up is the tunnel: it is pco's, and a change made to it in the dashboard is
overwritten in the next cycle. pco also publishes only HTTP and HTTPS to IPv4 addresses of
Proxmox guests, and of manual routes within `manualCIDRs`. For TCP, SSH, WARP, private
networks or anything else of `cloudflared`, run it yourself. A `cloudflared` of your own, with
its own tunnel, runs beside pco: pco never touches a tunnel it did not make, and its egress
filter looks only at the packets of `pco-connector`.
