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

**Agent**: The part of the daemon that serves one node of a cluster: its connectors, egress
filter, gateway and the proofs of its guests, from the desired state the controller sends it. A
node alone runs it in the daemon's process. See [Architecture](architecture.md#pco-on-a-cluster).

**Agent address**: The address of a node that the agents of the other nodes dial while it is the
controller, on TCP port 8644, chosen by `cluster.agentNetwork`. See
[Settings](settings.md#cluster).

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

**Attach**: Reaching the guests of a bridge or VLAN the node is not on through a leg of pco's
own put there, without touching the guests; `pco network attach` does it. See
[Networking](networking.md#attach).

## B

**Behind**: A controller whose store is older than what the agents remember, as after a
restore: it writes nothing until `pco promote`, in the appliance `pco appliance recover`. See
[Architecture](architecture.md#a-store-that-goes-back).

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

**Consent**: What lets pco put a card on the managed network on a guest: the registered tag
`cf-tunnel-managed`, an approval given with `pco managed approve`, or the setting
`managed.consent: auto` for a guest the gate tag admits that needs the network. See
[Networking](networking.md#consent).

**Controller**: The one node of a cluster that decides and writes: on a host the node that
holds the lease, in the appliance the controller container. See
[Architecture](architecture.md#pco-on-a-cluster).

**Copy**: In the appliance profile, a container that holds the store of an appliance but is not
the container it was installed as, such as a clone of it. A copy serves nothing. See
[Architecture](architecture.md#one-writer).

**Coverage**: Whether every node a cluster lists online runs an agent whose forwarding-table
watch runs, which `port (attested)` needs. See
[Identity](identity.md#port-attested-on-a-cluster).

**Credential**: A Cloudflare API token that pco stores, with an id and a label. See
[Cloudflare token](cloudflare-token.md).

**Cycle**: One pass of the daemon, every `pollInterval` and at once after an admin action: it
reads Proxmox and the store, decides, and brings Cloudflare, the connectors and the egress
filter in line. See [Architecture](architecture.md#the-reconcile-cycle).

## D

**Denylist**: The addresses pco never publishes, whatever the Notes say: those that cannot be a
guest's, the addresses of the nodes, and the gateways of SDN subnets. See
[Identity](identity.md#the-denylist-and-the-node).

**Drain**: Taking a node out of the replicas of the shared tunnel, for its maintenance, with
`pco node drain`; Proxmox's maintenance mode drains a node by itself. See
[Operations](operations.md#a-cluster).

## E

**Egress filter**: The nftables table `inet pco_egress`, which lets the connectors reach
Cloudflare's edge, the resolvers of the node, DNS over TLS at 1.1.1.1 and 1.0.0.1, and the
targets the daemon gives it, those of manual routes with `allowNode` among them; nothing else.
See
[Security](security.md#the-connector-egress-filter).

**Enforce**: The mode in which the daemon changes things at Cloudflare and on the node.
`pco apply` leaves observe-only for it. See
[Operations](operations.md#observe-only-until-pco-apply).

**Epoch**: The writer identity, a generation and a nonce, as one tenure of the writer drew it: on
a host each time a node takes the lease, in the appliance at each start of the container. See
[Architecture](architecture.md#epochs).

## F

**`filtered`**: The identity level of a guest on the managed network of the appliance: a proof
at `observed` through pco's leg, while the guest firewall of every card on the vnet is proven
to isolate it. See [Identity](identity.md#filtered-in-the-appliance).

**Foreign card**: A card on `pconet` that no attachment explains, as a clone or a restore copies
one. A consenting guest gets it replaced by a card of its own; any other has its link set down,
or is reported where pco may not change it. See [Networking](networking.md#clones-and-restores).

**`frozen`**: The state of a route in an account that pco leaves as it is, because a zone of the
account is in doubt. Nothing is changed at Cloudflare for that account. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

## G

**Gate tag**: The Proxmox tag that makes pco read the Notes of a guest, `cf-tunnel` unless the
setting `gateTag` names another. See [Annotations](annotations.md#where-routes-are-read) and
[Security](security.md#the-gate-tag-and-what-it-proves).

**Gateway**: What holds the legs and translates the service addresses to the guests behind
them: on a host the network namespace `pco`, kept by `pco-netns.service` and the daemon; in
the appliance the container's own cards and ruleset. See
[Networking](networking.md#legs-and-the-gateway).

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
`port`, `filtered` or `observed`, and `manual` for a route to an address, which names no guest. Also called the
proof level. See [Identity](identity.md#the-levels).

**`identityMinimum`**: The setting that names the lowest identity level at which a guest's
address is served, `port` by default. See [Identity](identity.md#identityminimum).

**Install**: One installation of pco, made by `pco setup`. Its id, 12 hexadecimal characters, is
in the name of its tunnels and the marker of its records. See
[Cloudflare token](cloudflare-token.md#what-pco-names-and-marks).

**Issue**: A mistake in the Notes of a guest or in the settings, with its position. `pco status`
lists them. See [Annotations](annotations.md#what-is-rejected-and-why).

## J

**Join token**: The token, good once and for 15 minutes, that an appliance agent container
enrols with, made by `pco cluster join-token` on the controller. See
[Appliance](appliance.md#a-cluster-of-appliances).

## L

**Lease**: On a host, the lock `/etc/pve/priv/lock/pco-leader` of the cluster filesystem; the node
that holds it is the controller. See [Architecture](architecture.md#the-lease).

**Leg**: A network card of pco's own in a network the node has no address in, with an address
of that network: `leg<N>` in the namespace `pco` of a host, a card `net<N>` of the appliance.
A guest behind a leg is proven and reached through it. See
[Networking](networking.md#legs-and-the-gateway).

## M

**Managed network**: The SDN vnet `pconet` in the zone `pco`, a network of pco's own on which
the guests that consent get a card with an address of `managed.subnet`, reached through pco's
leg and isolated from everything else. See [Networking](networking.md#managed).

**Mark**: The number from 1 to 15 of a network behind a leg, which names its leg, its
connection tracking zone, its routing table and its block of source ports. Fifteen is the
limit of networks in a cluster. See [Networking](networking.md#legs-and-the-gateway).

**Manual route**: A route made by root with `pco route manual add` or by an admin in the web
interface instead of in the Notes of a guest. It goes to a guest, whose address is proven as for
its Notes, or to an address within `manualCIDRs`, and then its level is `manual`: nothing proves
it. See [Security](security.md#manual-routes).

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

**`observed`**: The identity level of a guest on another node that this node cannot attest, of
every guest of the appliance, proven by ARP alone, and of a trusted static address. See [Identity](identity.md#the-levels).

**Owner**: Who a route belongs to: a guest, written `qemu/101` or `lxc/102`, or a manual route,
written `manual/<id>`. See [Annotations](annotations.md#hostnames-belong-to-one-guest-at-a-time).

## P

**Pinned tunnel**: A tunnel `pco-<install id>-<node>` whose connector runs on one node, for the
routes only that node can serve. See [Architecture](architecture.md#tunnels-and-placement).

**Placement**: Where a route of a cluster is served: the shared tunnel, or a tunnel pinned to a
node. See [Architecture](architecture.md#tunnels-and-placement).

**`port`**: The highest identity level: only the guest answers ARP for the address, and the
forwarding table of this node has its MACs on the guest's own port. See
[Identity](identity.md#the-levels).

**`port (attested)`**: On a cluster of hosts, `port` as another node proves it: on the word of
the guest's node, whose watch has the MAC on the guest's own port, and its own check of its
uplink. It counts as `port`. See [Identity](identity.md#port-attested-on-a-cluster).

**Profile**: The way pco is installed: `host`, on the node itself, or the appliance, in a
container. See [Profiles](profiles.md).

**Proof level**: The identity level. See [Identity](identity.md#the-levels).

## R

**`rejected`**: The state of a route for an apex or a wildcard that no `allowHosts` pattern
names. It takes no claim and gets no rule and no record. See
[Troubleshooting](troubleshooting.md#a-route-is-held-withdrawn-or-in-conflict).

**Replica**: A connector of the shared tunnel on one node; every active node runs one. See
[Architecture](architecture.md#tunnels-and-placement).

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

**Service address**: An address of `198.18.0.0/16` that stands for one guest address behind one
network: the connectors dial it, and the gateway translates it and sends the request out of
the leg. It is made for one owner and guest, never given twice, and the egress filter holds it
in place of the real address. See [Networking](networking.md#service-addresses).

**Sticky entry**: A forwarding entry of the bridge `pconet` of a host that keeps the MAC of a
managed guest's card on that guest's port, whatever another port sends, and waits on pco's leg
while the guest is stopped. See [Security](security.md#a-managed-guest-and-its-card-on-the-host).

**Served zone**: A zone pco publishes hostnames in, through the one credential that serves it.
See [Cloudflare token](cloudflare-token.md#several-accounts-and-zone-pins).

**Shared tunnel**: The tunnel `pco-<install id>` of an account, which runs a replica on every
active node of a cluster. See [Architecture](architecture.md#tunnels-and-placement).

**Soft-denied address**: The gateway of an interface of a node, or a resolver of a node. A
guest's route to it is served only once an approval names the address. See
[Identity](identity.md#routes-that-wait-for-an-admin).

**Store**: The state of pco in small JSON files under `/etc/pve/pco`, `/etc/pve/priv/pco` and
`/var/lib/pco`. See [Architecture](architecture.md#the-store).

## T

**Takeover hold**: The minute at most in which a new controller writes nothing while it waits
for every node's agent to report to it. See [Operations](operations.md#the-lease).

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
install, named by `/etc/pve/pco/meta/leader.json`: on a cluster, the controller. Its verdict, in `pco status`, is `ok`,
`stale`, `foreign` or `unknown`, and in the appliance `behind`, when its state is older than
its last write at Cloudflare. See [Architecture](architecture.md#one-writer).

## Z

**Zone pin**: A line of the setting `zonePins` that names the credential that serves a zone,
for a zone that more than one credential sees. See
[Cloudflare token](cloudflare-token.md#several-accounts-and-zone-pins).
