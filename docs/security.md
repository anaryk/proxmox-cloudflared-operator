# Security

This page says what pco trusts, what it does not, which parts hold which privileges,
and what each protection stops. It is written for the person who decides whether to run
pco on a node, and who would like to know where the limits are before an incident shows
them.

## What runs with which rights

| Part | Runs as | Can |
|---|---|---|
| The daemon, `pco.service` | root | Read Proxmox through its API token, open a raw socket to ask ARP, read the bridge forwarding table through netlink, run `systemctl` for the connectors and write their files, call the Cloudflare API with the stored tokens. It listens on a unix socket and nowhere else. |
| The connectors, `pco-cloudflared@<tunnel id>.service` | the system user `pco-connector`, with no capabilities | Open outbound connections, which the egress filter confines (below). |
| The `pco` command | whoever runs it | `setup`, `uninstall` and `pco egress` work on the node directly and need root. The others ask the daemon through its socket, which also lets only root in. |

The daemon is a root-equivalent part of the node. It runs as root, has the Cloudflare
tokens, manages systemd units, and answers a socket. Anyone who can make it do things has
the node. Its unit applies a few protections (`ProtectHome`, `PrivateTmp`,
`ProtectKernelModules`, `ProtectControlGroups`, `LockPersonality`) but no more, because
it needs systemd, nftables, raw sockets and files under `/var/lib` and `/etc/pve`.

The connector is where the confinement is. Its unit runs as `pco-connector` with
`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
`PrivateDevices`, the kernel-protection options, an empty capability set,
`RestrictAddressFamilies` limited to IPv4, IPv6, unix sockets and netlink, and
`RestrictNamespaces`. It cannot see `/etc/cloudflared`, `/usr/local/etc/cloudflared` or
`/root/.cloudflared`, and it is started with a configuration file of pco's own, so that
cloudflared reads no configuration of the host. Its token is handed to it as a systemd
credential and read from a file, so it never appears on a command line or in the
environment.

The Proxmox side is read-only. Setup makes the role `PCO`, the user `pco@pve` and the
token `pco@pve!pco`, and the role holds `VM.Audit`, `Sys.Audit`, `SDN.Audit` and
`VM.GuestAgent.Audit` (`VM.Monitor` on Proxmox VE 8.4), granted on `/`. The daemon never
changes a guest, a tag or a Notes field. Only `pco setup` and `pco uninstall` change
Proxmox, as root, and what they change is listed in the manifest.

The Cloudflare side is not read-only: the token can edit DNS records and tunnels in the
zones and accounts you chose. Scope it narrowly; see [Cloudflare token](cloudflare-token.md).

## The gate tag, and what it proves

A guest is published only if it carries the gate tag, `cf-tunnel`, and its Notes hold
routes. What does the tag prove?

When the tag is a registered tag, which setup arranges unless you decline, only a user
with `Sys.Modify` on `/` can set or remove it. The tag then proves that such a user tagged
that guest at some point. It does not prove more:

- It is not an approval of the hostnames. Whoever may edit the Notes of the guest
  (`VM.Config.Options`) chooses what the guest publishes, within the hostname policy
  (`allowHosts` and `denyHosts` in [Operations](operations.md)).
- It is not tied to what the guest is. A clone and a restore keep the tags and the Notes.
- If you decline to register the tag (`--no-registered-tags`), it proves nothing about
  who set it: whoever can edit the guest can tag it.

Setup also registers `cf-tunnel-managed`. Nothing reads it in this release.

### Clones and restores

A clone of a published guest carries the tag and the Notes, and so asks for the same
hostnames. It does not get them. The first guest to claim a hostname keeps it for as long
as it keeps asking, so the clone's route is in the state `conflict` and serves nothing.
Identity alone never moves a claim, and neither does a new guest that is created under the
VMID of an old one: the claim belongs to the guest `qemu/101`, and the new one inherits it.

A guest restored to a different VMID is a different owner and is in conflict while the
original exists. A guest that is restored or created again under the VMID of a published
guest publishes the same hostnames, without anyone approving it again, as long as it
carries the tag and the Notes. Admission mode `approve` closes that.

### Approval mode

With the setting `admission` set to `approve`, a tagged guest is published only after an
admin has approved it:

    pco guest list
    pco guest approve qemu/101
    pco guest revoke qemu/101

An approval is of the identity the guest has when it is approved: for a virtual machine its
SMBIOS UUID, or else its creation time, or else a hash of the MAC of its first card; for a
container a hash of the MAC of its first card. A guest that is created again under the same
VMID, and a clone, have another identity and need an approval of their own. The daemon
refuses an approval when the guest has changed since it was shown.

A guest that waits for approval is not published. A hostname it already holds stays
claimed and answers 503, so nobody else gets it. `pco status` shows how many guests wait, `pco guest
list` which, and `pco doctor` has a warning for each. Changing the mode to `approve`
takes the routes of every guest that is not approved off the air at the next cycle: their
hostnames answer 503.

Approval mode is recommended when the people who can edit the guests are not the people who
administer the node. It is off by default. The default is `tag`.

## Identity levels against four attackers

[Identity](identity.md) describes the checks. This table says what each level stops. Every
row but the first assumes a guest that carries the tag: a user cannot tag their own guest
when the tag is registered.

`port` is what a guest on this node gets. `observed` is what a guest on another node of the
cluster gets, and what an address behind a router gets when you trust it, and only if
`identityMinimum` is lowered to `observed`. `filtered` is reserved for another profile and
no host guest has it.

| Attacker | `observed` | `port` |
|---|---|---|
| A device on the uplink, outside Proxmox | Stops a device that answers for a guest's address with its own MAC. Does not stop one that copies the guest's MAC too: ARP cannot tell the copy from the guest, and for a guest on another node this node cannot see where the table sends its frames. | Stops both. The bridge must have learned the guest's MAC on the guest's own port and on no other. A MAC the table has on the uplink fails, and the route is withdrawn. Limit: the table is read once per cycle. |
| Root in another guest of this node | Stops a guest that answers with its own MAC, and a card configured with a MAC that a running guest has. Frames forged with the MAC of a guest of another node put that MAC on the port of the attacker's guest, which `observed` notices. | Stops forged frames as well. They move the entry to the attacker's port, so the victim's route is withdrawn (503); it is not taken over. That is a denial of service for the victim, not a hijack. Limit: the same. |
| A Proxmox user with `VM.Config.Network` on another guest | Does not stop it. The user can give their guest the MAC of any device on the segment that is not a running guest and, since the Notes are theirs, aim a route at that device. Only a MAC that a running guest has is refused. | Mostly stops it. To pass, the table has to have the device's MAC on the user's own port, which it has only after the user's guest sent a frame with that MAC and before the device sent its next one. A user who keeps transmitting with that MAC can have the check pass at the moment it is made, while the device wins the table in between. It is not a complete defence. |
| A Proxmox user with `VM.Config.Network` on the published guest | Cannot aim the route at another machine: the address must be one that the guest's own cards answer for. Can break the route, or move it between the guest's addresses, by editing its cards (MAC, bridge, VLAN, a new card). | The same. |

What this table shows is that `port` is meant to stand on its own and `observed` is not.
At `observed`, whoever can set the MAC of a guest's card and write its Notes can reach any
device on the segment. If you lower `identityMinimum`, give `VM.Config.Network` and Notes
access on tagged guests only to people you would trust with the whole segment, keep the tag
registered, and consider `admission: approve`.

Every check is made when pco looks, once in each cycle. In this release pco does not pin
neighbour entries of the node and does not react to a forwarding-table change between two
cycles, so the window an attacker can use is at least the length of the cycle (10 seconds by
default).

## The connector egress filter

A tunnel is configured at Cloudflare. Whoever can write that configuration, with a stolen
API token or in the dashboard, can make a connector open connections to anything the node
can reach, including the Proxmox web interface on port 8006. Checking addresses in pco does
not help against someone who writes the configuration behind its back. The egress filter
does: it confines the connector processes on the node, whatever the configuration says.

### What a connector can reach

The filter is the nftables table `inet pco_egress`. Its chain sits on the output hook at
priority -10, after connection tracking and the destination NAT, before the filter chains
of the Proxmox firewall, and it applies only to packets of sockets that belong to the user
`pco-connector`. The uid is looked up by name, because it differs from node to node. Every
other process is untouched.

For a connector's packet the chain does the following, in this order, and the first rule
that matches decides:

1. A packet in an invalid connection state is dropped.
2. Replies of TCP connections to an address of the node itself are accepted. That is how
   the daemon reads the `/ready` endpoint of a connector on 127.0.0.1.
3. An address on the block list of this node is rejected on every port.
4. The targets are accepted: a TCP connection to an address and port in the targets set,
   which holds the origins the daemon verified.
5. The resolvers of the node are accepted: the `nameserver` lines of `/etc/resolv.conf`, TCP
   and UDP port 53.
6. Anything else addressed to the node itself is rejected and counted.
7. TCP and UDP port 7844 to a public unicast address is accepted. That is Cloudflare's
   edge. The rule excludes the ranges that are not the public internet (private, shared,
   link-local, loopback, benchmark, documentation, translation prefixes, 6to4, Teredo,
   multicast and reserved ones, for IPv4 and IPv6) rather than list Cloudflare's addresses,
   so there is no list to keep up to date.
8. TCP port 853 to 1.1.1.1 and 1.0.0.1 is accepted: the DNS over TLS fallback of cloudflared.
9. Everything else is rejected and counted. TCP is answered with a reset and the rest with
   ICMP administratively prohibited, not dropped, so that a connection to a withdrawn target
   fails at once instead of waiting for a timeout.

A connector can therefore reach Cloudflare's edge, the resolvers, and the verified targets,
and cannot reach the management ports of the node or any other host. A rule written at
Cloudflare for `http://10.0.0.1:8006` gets a refused connection.

The daemon's own checks, among them the request that `pco diagnose` makes, come from the
daemon and not from a connector, so the filter does not confine them.

### The table at boot

`pco-egress.service` loads the table before the daemon starts, with the resolvers of the node
and no targets, and every connector unit requires it and starts after it. A connector can
therefore not run without the table, and a table that fails to load keeps the connector from
starting. After a reboot a connector reaches Cloudflare's edge and the resolvers, and
nothing else, until the daemon has verified the targets and added them.

The table is loaded in one nft transaction, so it is whole or not there. The commands that
read it check not only the targets but the whole table, its chains, rules and counters, and
say when it is not what pco loads.

### The `pco egress` commands

They work on the node directly, without the daemon, and need root.

| Command | What it does |
|---|---|
| `pco egress show` | Says whether the filter is on, off, not loaded, or loaded but not as pco loads it (it then names what differs). Lists the targets, the resolvers, the addresses blocked on this node, and what the table rejected since it was last loaded in full, in packets: to addresses of this node, and to anything else. Exits 1 when the filter is off, gone or changed. |
| `pco egress block <address>` | Puts the address on the block list of this node and takes its entries out of the table at once. The list is kept in `/var/lib/pco/egress-blocked.json` and is subtracted from every set the daemon loads, until `unblock`. It needs no daemon and no Cloudflare. |
| `pco egress unblock <address>` | Takes the address off the list. If it is still a verified target, the daemon puts it back at its next cycle. |
| `pco egress off` | Removes the table and writes `/var/lib/pco/egress-off.json`. While that file exists, neither the boot unit nor the daemon loads the table. The connectors are not confined. |
| `pco egress on` | Removes the switch and loads the table, with the resolvers and no targets unless the table is there as it should be. |
| `pco egress load` | Loads the table when it is gone or not as it should be, and leaves an intact one with its sets as they are. It is what the boot unit runs. It is not listed by `pco egress --help`. |

An example of `pco egress show` on a node with three targets and one blocked address:

    The egress filter is on.

    Targets:
      10.0.0.5:80
      10.0.0.5:8080
      10.0.0.6:443
    Resolvers:
      192.168.1.1

    Rejected since the table was last loaded in full:
      to addresses of this node  3 packets
      to anything else           12 packets

    Blocked on this node:
      10.0.0.9

### The off switch

`pco egress off` exists for the case that the filter is itself the fault, and for nothing
else, because with it the connectors can reach everything the node can. It is local root
only: no request of the daemon's API can switch the filter off, and the switch is a file
that only root can write. `pco egress show` says that the filter is off, since when, and exits 1.

### If the table disappears

`nft flush ruleset`, and an enabled `nftables.service` whose configuration begins with
`flush ruleset` (which the Debian default does) when it is started or reloaded, remove the
table. The connectors are then not confined until it is loaded again. `pco egress show` says
`The egress filter is on, but its table is not loaded: the connectors are not confined`.

To load it again, run

    pco egress load

(`pco egress on`, which `pco egress show` suggests, loads it too, unless the filter was
switched off.) Do not restart `pco-egress.service` for that. The connector units require it, and a restart
of a unit that others require restarts them as well, so every connector would drop its
connections to Cloudflare. For the same reason, do not stop it. `pco egress load` is the
way to reload: it loads the table when it is gone or changed, and does nothing to one that
is intact.

## Secrets and where they live

| What | Where | Notes |
|---|---|---|
| The secret of the Proxmox API token `pco@pve!pco` | `/etc/pve/priv/pco/meta/pve-token.json` | Read by the daemon when it starts. |
| Cloudflare API tokens | `/etc/pve/priv/pco/credentials/<id>.json` | One file for each credential. |
| Tunnel run tokens of the connectors | `/var/lib/pco/tunnels/<tunnel id>.token` | Mode 0600 in a directory of mode 0700. Passed to cloudflared as a systemd credential. |

`/var/lib/pco` is mode 0700. `/etc/pve` is the cluster filesystem, and Proxmox keeps
`/etc/pve/priv` readable by root alone. On a cluster it is replicated to every node, so every node holds these files.
Anything that backs up `/etc/pve` or `/var/lib/pco` copies the secrets with it: protect
those backups as you protect the tokens.

pco never takes a token from an argument or the environment, and prints or encodes it as
`[redacted]` when it must show a value. The daemon's API does not return tokens, error
messages from Cloudflare have the token blanked out, and a token file that others can read
makes the command that reads it warn.

The daemon talks to the Proxmox API on `https://127.0.0.1:8006`. Port 8006 is not a
privileged port, so while `pveproxy` is down any local user could listen on it and be handed
the token. To prevent that, pco does not trust the name on the certificate: it requires the
loopback API to present, byte for byte, the certificate that this node serves, which it reads
from `/etc/pve/local/pveproxy-ssl.pem` or else `pve-ssl.pem`. A certificate that no longer
matches the one pco read is read again before it is refused, in case you replaced it.

The rest of the state (the install id, the writer identity, the settings, the claims) is not
secret; [Operations](operations.md) lists it.

## The socket API and who may call it

The daemon answers on the unix socket `/run/pco/pco.sock`. It has no network listener. The
directory `/run/pco` has mode 0750 and the socket 0660, both owned by root; the group is
`pco-web` when that group exists, and root's group otherwise. The daemon also reads the
user id of the process on the other end of every connection from the kernel and answers only
root, and the user `pco-web` if there is one. Anyone else gets a refusal that says nothing
about which requests exist. `pco` run as another user reports that it must run as root and
exits 2.

The daemon refuses to make its socket directory unless it is named `pco`, lives in a
directory that only the daemon's user can write, and is not a link. The check is there so
that a socket path cannot be pointed at a directory such as `/tmp` whose mode and owner the
daemon would then change.

Everything a request can do is what the CLI shows: read the state, ask for a cycle, apply,
adopt, add and check and remove credentials, resolve claims, approve guests, diagnose, run
the doctor. A request cannot read a token, and it cannot change the egress filter.

## What Cloudflare sees

- The hostnames you publish, and the addresses and ports of the guests behind them. They are
  in the ingress rules of the tunnel, such as `http://10.0.0.11:3000`.
- The install id, in the names of the tunnel and the marker of the DNS records, and the
  writer's generation and nonce in the sentinel rule.
- API requests from the node's address, with the user agent `pco/<version>`.
- The connector's outbound connections from the node's address.
- The traffic of every proxied hostname, as for any proxied hostname at Cloudflare: it ends at
  Cloudflare's edge in the clear, and from the connector to the guest it is plain HTTP unless
  the route says `https`.

It does not see the Proxmox token, the other guests, the Notes beyond the hostnames and
route options, or anything that is not published.

A hostname you publish is public, as is the tunnel it points at (the CNAME holds the tunnel's
id). pco puts nothing in front of it: no Cloudflare Access policy, no login. What protects an
application is the application, or whatever you configure at Cloudflare for the hostname
yourself.
