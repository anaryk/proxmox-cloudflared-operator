# Glossary

The words these pages use in a sense of their own, each with where it is explained. A word
in code type, such as `held`, is a value pco shows or reads as it is written.

## A

**`active`**: The state of a route that is served: its rule sends requests to its verified
address, and its hostname has a record. See
[Troubleshooting](troubleshooting.md#reading-pco-status).

**Admin**: Who may change what pco does: root on the node, through the command line, and in the
web interface a Proxmox user or token with `Sys.Modify` on `/`. A reader, with `Sys.Audit` on
`/`, can only look. See [Architecture](architecture.md#the-web-process).

**Admission**: The setting `admission`. With `tag`, a guest that carries the gate tag is
published; with `approve`, it also needs an approval. See [Security](security.md#approval-mode).

**`allowHosts`, `denyHosts`**: Patterns of the hostnames that may and may not be published. An
apex and a wildcard are published only when an `allowHosts` pattern names them. See
[Operations](operations.md#settings).

**`allowNode`**: An option of a manual route that lets it point at an address of a node, which
the denylist otherwise refuses. See [Security](security.md#manual-routes).

**Apex**: The name of a zone itself, such as `example.com`. A guest publishes it only when an
`allowHosts` pattern names it. See [Annotations](annotations.md#hostnames).

**Appliance**: The profile in which pco and its connectors run in an unprivileged container,
with nothing installed on the node; `install.sh --appliance` installs it. See
[Appliance](appliance.md).

**Approval**: What `pco guest approve` records of a guest: its identity, and the MACs and
addresses it was shown with. It admits the guest in admission mode `approve`, and releases its
routes that wait for an admin in either mode. See [Security](security.md#approval-mode) and
[Identity](identity.md#routes-that-wait-for-an-admin).

## B

**Binding**: The address pco verified for a hostname, with the MAC, the identity level and the
time of the proof. Bindings are kept in `/var/lib/pco/bindings`, and a bound address is tried
first in the next cycle. See [Identity](identity.md#candidates-and-their-order).

## C

**Candidate**: An address that may become the target of a route: the one the route names, or
the static and the reported addresses of the guest's network cards. See
[Identity](identity.md#candidates-and-their-order).

**Claim**: The record of which owner holds a hostname. The first guest to claim a hostname keeps
it for as long as it asks for it, and for the grace period after. See
[Annotations](annotations.md#hostnames-belong-to-one-guest-at-a-time).

**`conflict`**: The state of a route whose hostname another owner holds. It serves nothing. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Connector**: A `cloudflared` process that runs one tunnel on the node, as the unit
`pco-cloudflared@<tunnel id>.service` and the user `pco-connector`. See
[Architecture](architecture.md#the-connectors).

**Copy**: In the appliance profile, a container that holds the store of an appliance but is not
the container it was installed as, such as a clone of it. A copy serves nothing. See
[Architecture](architecture.md#one-writer).

**Credential**: A Cloudflare API token that pco stores, with an id and a label. See
[Cloudflare token](cloudflare-token.md).

**Cycle**: One pass of the daemon, every `pollInterval` and at once after an admin action: it
reads Proxmox and the store, decides, and brings Cloudflare, the connectors and the egress
filter in line. See [Architecture](architecture.md#the-reconcile-cycle).

## D

**Denylist**: The addresses pco never publishes, whatever the Notes say: those that cannot be a
guest's, the addresses of the nodes, and the gateways of SDN subnets. See
[Identity](identity.md#the-denylist-and-the-node).

## E

**Egress filter**: The nftables table `inet pco_egress`, which lets the connectors reach
Cloudflare's edge, the resolvers of the node, DNS over TLS at 1.1.1.1 and 1.0.0.1, and the
targets the daemon gives it, those of manual routes with `allowNode` among them; nothing else.
See
[Security](security.md#the-connector-egress-filter).

**Enforce**: The mode in which the daemon changes things at Cloudflare and on the node.
`pco apply` leaves observe-only for it. See
[Operations](operations.md#observe-only-until-pco-apply).

**Epoch**: In the appliance profile, the writer identity drawn at a start of the container: the
generation one up and a new nonce. See [Architecture](architecture.md#one-writer).

## F

**`frozen`**: The state of a route in an account that pco leaves as it is, because a zone of the
account is in doubt. Nothing is changed at Cloudflare for that account. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

## G

**Gate tag**: The Proxmox tag that makes pco read the Notes of a guest, `cf-tunnel` unless the
setting `gateTag` names another. See [Annotations](annotations.md#where-routes-are-read) and
[Security](security.md#the-gate-tag-and-what-it-proves).

**Generation**: The number in the writer identity. A recovery takes one above every generation
it finds at Cloudflare, and a writer stops when it finds a newer one it does not know. See
[Architecture](architecture.md#one-writer).

**Grace**: How long a hostname must be unwanted before its record is removed, and how long a
claim is kept after its holder stops asking: the setting `grace`, a minute by default. See
[Operations](operations.md#grace-periods-and-the-mass-delete-guard).

## H

**`held`**: The state of a hostname that is claimed and that nobody serves, such as one its
holder still names in a broken entry. It answers 503. Not to be confused with a cycle that
holds. See [Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Hold**: What a cycle does when it cannot be sure of what it read: it changes nothing at
Cloudflare and on the connectors, and `pco status` says why. See
[Operations](operations.md#holding-back).

## I

**Identity level**: How strongly pco proved that an address belongs to the guest of a route:
`port`, `filtered` or `observed`, and `manual` for a route without a guest. Also called the
proof level. See [Identity](identity.md#the-levels).

**`identityMinimum`**: The setting that names the lowest identity level at which a guest's
address is served, `port` by default. See [Identity](identity.md#identityminimum).

**Install**: One installation of pco, made by `pco setup`. Its id, 12 hexadecimal characters, is
in the name of its tunnels and the marker of its records. See
[Cloudflare token](cloudflare-token.md#what-pco-names-and-marks).

**Issue**: A mistake in the Notes of a guest or in the settings, with its position. `pco status`
lists them. See [Annotations](annotations.md#what-is-rejected-and-why).

## M

**Manual route**: A route that names an address and no guest, made by root with
`pco route manual add` or by an admin in the web interface, within `manualCIDRs`. Its level is
`manual`: nothing proves it. See [Security](security.md#manual-routes).

**Marker**: The comment `pco:<install id>` on every DNS record pco made. pco changes only records
that carry it. See [Cloudflare token](cloudflare-token.md#what-pco-names-and-marks).

**Mass delete guard**: The rule that holds the removal of many records at once until an admin
confirms it with `pco apply --confirm-deletes`. See
[Operations](operations.md#grace-periods-and-the-mass-delete-guard).

## N

**`no-zone`**: The state of a route whose hostname is in no zone a credential serves. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Nonce**: The random string in the writer identity, which tells two writers of one generation
apart. See [Architecture](architecture.md#one-writer).

## O

**Observe-only**: The mode of a new install and of one after `pco setup --recover`: the daemon
works out what it would do and changes nothing at Cloudflare. `pco apply` ends it. See
[Operations](operations.md#observe-only-until-pco-apply).

**`observed`**: The identity level of a guest on another node, proven by ARP alone, and of a
trusted static address. See [Identity](identity.md#the-levels).

**Owner**: Who a route belongs to: a guest, written `qemu/101` or `lxc/102`, or a manual route,
written `manual/<id>`. See [Annotations](annotations.md#hostnames-belong-to-one-guest-at-a-time).

## P

**`port`**: The highest identity level: only the guest answers ARP for the address, and the
forwarding table of this node has its MACs on the guest's own port. See
[Identity](identity.md#the-levels).

**Profile**: The way pco is installed: `host`, on the node itself, or the appliance, in a
container. See [Profiles](profiles.md).

**Proof level**: The identity level. See [Identity](identity.md#the-levels).

## R

**`rejected`**: The state of a route for an apex or a wildcard that no `allowHosts` pattern
names. It takes no claim and gets no rule and no record. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Rogue connector**: A connector that Cloudflare lists on a tunnel of this install and that pco
does not run on the node; the pages call it a connector that is not pco's. It takes a share of
the requests, and pco reports it. See [Security](security.md#a-connector-that-is-not-pcos).

**Route**: A hostname with the target it is served from and its options, written in the Notes of
a guest or made as a manual route. See [Annotations](annotations.md).

**Run token**: The token a connector runs its tunnel with, which the daemon reads from Cloudflare.
It is not the API token you give pco. See
[Cloudflare token](cloudflare-token.md#where-the-token-is-kept).

## S

**Segment**: A bridge and a VLAN, named `vmbr0`, or `vmbr0:20` for VLAN 20. A route proven by
ARP at `observed` is served only on a segment an admin acknowledged. See
[Identity](identity.md#routes-that-wait-for-an-admin).

**Sentinel**: The rule `g<generation>.<nonce>.pco-<install id>.invalid` in the configuration of
every tunnel, which records which writer wrote it. See [Architecture](architecture.md#one-writer).

**Served zone**: A zone pco publishes hostnames in, through the one credential that serves it.
See [Cloudflare token](cloudflare-token.md#several-accounts-and-zone-pins).

**Soft-denied address**: The gateway of an interface of a node, or a resolver of a node. A
guest's route to it is served only once an approval names the address. See
[Identity](identity.md#routes-that-wait-for-an-admin).

**Store**: The state of pco in small JSON files under `/etc/pve/pco`, `/etc/pve/priv/pco` and
`/var/lib/pco`. See [Architecture](architecture.md#the-store).

## T

**Target**: The address and port on a guest that a route sends requests to. The targets the
daemon verified are what the egress filter lets the connectors reach. See
[Security](security.md#how-the-daemon-keeps-the-targets).

**Trusted static address**: A static address of a guest behind a router, served without ARP when
`trustStatic` is on and it lies in `trustedCIDRs`. Its level is `observed`. See
[Identity](identity.md#static-addresses-behind-a-router).

**Tunnel**: The Cloudflare Tunnel `pco-<install id>` that pco makes in each account with a zone
that has routes, and whose configuration it alone writes. See
[Cloudflare token](cloudflare-token.md#what-pco-names-and-marks).

## U

**`unreachable`**: The state of a route that is not served for want of an address: none passed
its identity check, or the one that did is held back, or does not answer on its port. See
[Troubleshooting](troubleshooting.md#a-route-is-unreachable).

## V

**Verified**: Of an address, proven to be the guest's. Of a tunnel, its configuration at
Cloudflare was read back and equals the plan. See
[Troubleshooting](troubleshooting.md#reading-pco-status).

## W

**Watch**: What the daemon runs between two cycles: it follows the neighbour table and the
forwarding tables of the bridges, and takes an address out of the egress filter as soon as its
MAC moves. See [Security](security.md#between-two-cycles).

**Wildcard**: A hostname that starts with `*.`, such as `*.example.com`. A guest publishes it
only when an `allowHosts` pattern names it. See
[Annotations](annotations.md#wildcards-and-their-order).

**`withdrawn`**: The state of a route that was served and lost its identity, or whose guest
stopped. It answers 503 and keeps its record. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Writer**: The one process that may write the tunnel configurations and the DNS records of an
install, named by `/etc/pve/pco/meta/leader.json`. Its verdict, in `pco status`, is `ok`,
`stale`, `foreign` or `unknown`, and in the appliance `behind`, when its state is older than
its last write at Cloudflare. See [Architecture](architecture.md#one-writer).

## Z

**Zone pin**: A line of the setting `zonePins` that names the credential that serves a zone,
for a zone that more than one credential sees. See
[Cloudflare token](cloudflare-token.md#several-accounts-and-zone-pins).
