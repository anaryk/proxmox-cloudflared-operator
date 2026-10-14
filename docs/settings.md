# Settings

This page lists every setting of pco: what it is, its default, what it accepts, what it
changes, when a change takes effect and where to change it. The settings are one file,
`/etc/pve/pco/meta/settings.json` on a host, which `pco setup` makes with the defaults.
The daemon reads it at the start of each cycle.

## Changing the settings

Show them, edit them in a file and save the file:

    pco settings show
    pco settings show --json > settings.json
    pco settings apply settings.json

`pco settings show` prints each setting with its value, its range where it has one, and
`read at start` where a change needs a restart of the daemon. With `--json` it prints what
the daemon sent, and that is the file `pco settings apply` takes:

```json
{
  "rev": 1,
  "settings": {
    "gateTag": "cf-tunnel",
    "pollInterval": "10s",
    "grace": "1m0s",
    "manualCIDRs": ["10.0.5.0/24"],
    "admission": "approve",
    "observeOnly": false,
    "identityMinimum": "port",
    "maxHostnamesPerGuest": 32,
    "reverifyInterval": "1m0s",
    "cloudflareBudget": 1000,
    "managed": { "consent": "per-guest", "subnet": "10.203.118.0/24", "detachGrace": "10m0s" }
  },
  "readAtStart": ["cloudflareBudget", "gateTag", "trustStatic", "trustedCIDRs"],
  "limits": {
    "cloudflareBudget": { "min": 100, "max": 1150 },
    "grace": { "min": "30s" },
    "managed.detachGrace": { "min": "1m0s" },
    "maxHostnamesPerGuest": { "min": 1 },
    "pollInterval": { "min": "5s" },
    "reverifyInterval": { "min": "10s", "max": "5m0s" }
  },
  "notes": []
}
```

Edit what is under `settings` and leave the rest. A setting that is empty is left out of the
file, so a list such as `allowHosts` is there only once it has an entry; add the key to set
it. The `rev` is the revision the settings were read at: the daemon saves the file only while
the stored settings still have it, and otherwise says that they changed since, and you read
them again and edit those. A key that is not a setting, a value out of its range and a
pattern that does not normalise are refused with the name of the setting. Run as root: the
command asks the daemon on its socket.

The web interface has the same settings on its Settings page, in sections, with the range and
the default beside each one and a review of what changes before it saves, at the revision the
page read them at, as `pco settings apply` does. Only an admin can save; a reader can read the
page and export the settings as a file. The last column of the tables below names the section
of that page where each setting is. The page also exports the settings with the manual routes as
one file and imports such a file, and after a save of a setting read at start it offers to
restart the daemon. [Web interface](web-ui.md#settings) has the page.

A settings file can also be edited in place on the node. It is the object in `data`; leave
the rest of the envelope as it is (see [Files](files.md)), and run `python3 -m json.tool`
over it to check the syntax. A command is the safer way: it validates before it saves, and
the daemon refuses a file it cannot read, as described [below](#a-settings-file-the-daemon-cannot-use).

### When a change takes effect

The daemon reads the file again at the start of each cycle, so most changes take effect at
the next one. Four settings are wired when the daemon starts: `gateTag`, `trustStatic`,
`trustedCIDRs` and `cloudflareBudget`. A change to those is saved, shown as a problem of
`pco status`,

```text
settings gateTag changed since pco started and are read only at start; restart pco (systemctl restart pco) for them to take effect
```

and applies after `systemctl restart pco`. `pco settings apply` and the web page name them
when they save. The connectors keep running through a restart of the daemon.

`pco settings apply` does not leave observe-only mode. `pco apply` does, and shows first
what that changes; a file that has `"observeOnly": false` where the stored settings have
`true` is refused. Going back to observe-only is a setting like another.

### Compatibility

A new setting is optional and has a safe default, so a file written before it keeps working,
and a field the file leaves out has its default. Settings are read strictly: a build that
does not know a field refuses a file that has it, so an older build refuses the settings a
newer one saved. The same holds for the schema version of every file, which is 1: a file
written by a newer schema is refused and never rewritten. Downgrades are not supported (see
[Operations](operations.md#upgrades)).

## Publishing

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `observeOnly` | boolean, `true` | `true`, `false` | Next cycle | Publishing |
| `gateTag` | string, `cf-tunnel` | a Proxmox tag | At start | Gate tag |
| `admission` | string, `tag` | `tag`, `approve` | Next cycle | Admission |
| `allowHosts` | patterns, none | `*`, a hostname, `*.` and labels | Next cycle | Hostname policy |
| `denyHosts` | patterns, none | as `allowHosts` | Next cycle | Hostname policy |
| `maxHostnamesPerGuest` | integer, `32` | at least `1` | Next cycle | Limits |
| `manualCIDRs` | IPv4 prefixes, none | as `10.0.5.0/24` | When a route is made or changed | Manual routes |

- `observeOnly`: whether the daemon only observes. In observe-only mode it runs every step up
  to the plan and shows what it would do (`pco plan`, every action held by `observe mode`),
  and changes nothing at Cloudflare: no tunnel, no configuration, no record, no connector. A
  new install, and one after `pco setup --recover`, starts in it. `pco apply` leaves it. On
  the web page, the Publishing section has the button Return to observe-only, and the plan
  page leaves it.
- `gateTag`: the tag that makes a guest a candidate for publishing. A Proxmox tag: lower-case
  letters, digits and `_ - + .`, not starting with `-`, `+` or `.`, at most 64 characters.
  After you change it, run `pco setup` (or `pco setup --repair`) on the node, which registers
  the new tag in Proxmox beside `cf-tunnel` and `cf-tunnel-managed`; see
  [Security](security.md). In the appliance, `pco appliance init` stores the gate tag the
  installer was given in the settings of a new install; the settings of an existing install
  are kept, and init says when the tag it was given is another.
- `admission`: `tag` publishes a guest that carries the gate tag. `approve` also needs the
  approval of an admin (`pco guest approve`). Use `approve` when anyone but the admins holds
  `VM.Clone` on a tagged guest or template: Proxmox copies the tag to the clone.
- `allowHosts`: the hostnames that may be published. Empty allows every name but the apex of
  a zone and a wildcard, which a guest publishes only when a pattern names them; see
  [the patterns](#hostname-patterns). The web page labels it Allowed hostnames.
- `denyHosts`: the hostnames that may not be published. A deny rule wins over an allow rule.
  The web page labels it Denied hostnames.
- `maxHostnamesPerGuest`: how many hostnames the Notes of one guest may name. A guest that
  names more publishes none of them, keeps the ones it holds, and an issue says so. A value
  below 1 in the file is read as 1. The web page labels it Hostnames per guest.
- `manualCIDRs`: the prefixes the address of a manual route must lie in. It is checked when a
  route is made or changed, so a change needs no restart. With none, no route to an address
  can be made. An address of a node needs its prefix listed here as well as the route's
  `allowNode` option. `trustedCIDRs` has no say in this. The web page labels it Prefixes of
  manual route addresses.

## Timing

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `pollInterval` | duration, `10s` | at least `5s` | Next cycle | Timing |
| `grace` | duration, `1m0s` | at least `30s` | Next cycle | Timing |

- `pollInterval`: the time between two cycles. Cycles closer together would only ask Proxmox
  and Cloudflare more. A value below 5 seconds in the file is read as 5 seconds.
- `grace`: how long a removal waits before it is made: a DNS record of a hostname that is no
  longer wanted, and the claim of a guest that stops asking for it. A grace of a moment would
  remove a record as soon as one cycle missed its name. A value below 30 seconds in the file
  is read as 30 seconds.

A duration is written as Go reads it, `30s`, `90s`, `2m`, `1m30s`. The daemon writes it back
as `1m0s`.

## Identity

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `identityMinimum` | string, `port` on a host, `observed` in the appliance | `port`, `filtered`, `observed` | Next cycle | Identity |
| `reverifyInterval` | duration, `1m0s` | `10s` to `5m0s` | Next cycle | Timing |
| `trustStatic` | boolean, `false` | `true`, `false` | At start | Identity |
| `trustedCIDRs` | IPv4 prefixes, none | IPv4 prefixes | At start | Identity |

- `identityMinimum`: the lowest identity level at which the address of a guest is served; a
  route below it is held back and answers 503. `port` is the strictest. Guests of other nodes
  and trusted static addresses reach `observed` only. On a host `filtered` asks for `port`: it
  is the level of the appliance's managed network. See
  [Identity](identity.md#the-appliance-and-observed). The default is `port` on a host.
  `pco appliance init` stores `observed` in the settings of a new appliance,
  because a container cannot read the forwarding table of the bridge: `port` is beyond it, and
  its best level on an ordinary network is `observed`. Raising the setting there holds back
  every route the container cannot prove at the level asked, which on an ordinary network is
  all of them. The web page labels it Lowest identity level served.
- `reverifyInterval`: how long a proof of identity that the watch of the network vouches for
  stands before the address is checked on the wire again. A shorter time costs more ARP
  exchanges; see [Operations](operations.md#how-often-an-address-is-checked-on-the-wire). The
  web page labels it Proof of identity stands for.
- `trustStatic`: trust the static addresses of guests that a gateway routes to, in the
  prefixes of `trustedCIDRs`. Such an address is proven at `observed` only, so it is served
  only when `identityMinimum` is `observed` as well. See
  [Identity](identity.md#static-addresses-behind-a-router). The web page labels it Trust
  static addresses behind a router.
- `trustedCIDRs`: the prefixes in which static addresses are trusted. Keep them as narrow as
  you can: whoever edits the network configuration of a tagged guest can give a card any
  address inside them. The web page labels it Prefixes of trusted static addresses.

## Cloudflare

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `zonePins` | zone name to credential id, none | `{ "example.com": "a1b2c3d4" }` | Next cycle | Zones |
| `cloudflareBudget` | integer, `1000` | `100` to `1150` | At start | Limits |

- `zonePins`: the credential that serves a zone seen through several. A zone seen by more
  than one credential, none of which served it alone before, is left as it is until it is
  pinned. A pin to a credential that does not see the zone leaves the zone as it is. The id is
  that of a stored credential (`pco credential list`). Zone names are lower-cased, and two keys
  that name one zone are refused. See [Cloudflare token](cloudflare-token.md). On the web page,
  the Zones section has the pins as lines of a zone and a credential id, and Pin on the page of
  the zones sets the pin of one zone.
- `cloudflareBudget`: how many requests in 5 minutes pco spends of each credential, of the
  1200 that Cloudflare allows a user. What is left is for the dashboard and other tools.
  Credentials of one Cloudflare user share the 1200, so give two such credentials budgets that
  add up to no more. See [Operations](operations.md#what-cloudflares-rate-limit-costs). The
  web page labels it Cloudflare budget.

## Managed network

The settings of pco's managed network, the SDN vnet `pconet` on which guests that consent get
a card of pco's ([Networking](networking.md#managed)), are the keys of one object, `managed`:

```json
"managed": {
  "consent": "per-guest",
  "subnet": "10.203.118.0/24",
  "detachGrace": "10m0s"
}
```

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `managed`: `consent` | string, `per-guest` | `per-guest`, `auto` | Next cycle | None; edit the file |
| `managed`: `subnet` | IPv4 prefix, `10.203.118.0/24` | a `/16` to a `/28` | Next cycle | None; edit the file |
| `managed`: `detachGrace` | duration, `10m0s` | at least `1m0s` | Next cycle | None; edit the file |
| `managed`: `acceptFirewallImpact` | boolean, `false` | `true`, `false` | Next cycle | None; edit the file |

- `consent`: which guests get a card on the managed network. `per-guest` gives one to a
  guest that carries the tag `cf-tunnel-managed` or that an admin approved. `auto` also gives
  one, without either, to a guest the gate tag admits whose routes need the network. `auto`
  can be set only while the gate tag is registered as admin-only, and a `gateTag` change to a
  tag that is not registered is refused while it is on; when the tag loses its registration
  later, automatic attaches stop and the guests that are attached stay. A guest whose Notes
  say `cf-tunnel-managed=off` gets none in either mode; see
  [Annotations](annotations.md#refusing-the-managed-network) and
  [Networking](networking.md#consent).
- `subnet`: the addresses of the managed network. pco's own leg has the first, the guests the
  others up to the last before the broadcast address. A `/16` to a `/28`, written as its
  network address, as `10.203.118.0/24` and not `10.203.118.1/24`. It may not overlap the
  service prefix `198.18.0.0/16`, whose addresses the connectors dial, nor a range that never
  reaches a guest or is set aside: `0.0.0.0/8`, loopback, link-local, multicast, `240.0.0.0/4`,
  `192.0.0.0/24`, the documentation ranges and `198.18.0.0/15`. The daemon changes it only while
  the managed network and its leases do not exist, and refuses a subnet that overlaps a route
  of a node, an address of a guest, static or reported, or the prefix of an attached network,
  naming what it overlaps (`pco settings apply` and the API say so with the field
  `managed.subnet`).
- `detachGrace`: how long a card stays on a guest after its consent is gone, before pco
  removes it. Taking a card off changes the guest's hardware, so it waits at least a minute;
  consent that comes back within the time keeps the card.
- `acceptFirewallImpact`: in the appliance, the admin's confirmation, for every guest at once,
  that pco may turn on the firewall of a guest given its card by the tag or by `auto` while
  another card of that guest has `firewall=1`, which the firewall then filters too. A host
  ignores it.

A key the file leaves out has its default, and so has the whole object when the file leaves
it out. A key that is there is taken as written: one that is empty or zero, as
`"detachGrace": "0s"`, is refused with its path, `managed.detachGrace`, and never read as
the default. This holds for the file on the node, `pco settings apply`, the API and the web
page alike.

## Cluster

The settings of a cluster of nodes are the keys of one object, `cluster`. They are for pco
on more than one node ([Operations](operations.md#a-cluster)); a node alone reads them and has
no use for them. While an upgrade from an earlier release is not finished, the file cannot hold
the object, as that release would refuse it: `pco cluster finish-upgrade` comes first.

```json
"cluster": {
  "agentNetwork": "10.99.0.0/24",
  "replicas": ["pve1", "pve2"],
  "followMaintenance": true,
  "rotateOnForget": true,
  "admissionMaxMoves": 10
}
```

| Setting | Type, default | Accepts | Takes effect | Web UI |
|---|---|---|---|---|
| `cluster`: `agentNetwork` | prefix, none | an IPv4 or IPv6 prefix, as `10.99.0.0/24` | At start of each node's daemon | None; edit the file |
| `cluster`: `replicas` | node names, none | at most 25 names of nodes, each once | Next cycle | None; edit the file |
| `cluster`: `followMaintenance` | boolean, `true` | `true`, `false` | Next cycle | None; edit the file |
| `cluster`: `rotateOnForget` | boolean, `true` | `true`, `false` | When a node is forgotten | None; edit the file |
| `cluster`: `admissionMaxMoves` | integer, `10` | `1` to `1000` | Next cycle | None; edit the file |

- `agentNetwork`: the network of each node's agent address, written as its network address:
  the address a node listens on for the agents of the other nodes while it is the controller,
  which they dial, and which the firewall's ipset `pco-nodes` holds. Left out, it is the address
  the node's name resolves to. Keep it off the networks of corosync, which `pco doctor` warns
  about, and restart pco on every node after a change.
- `replicas`: the nodes that run the connectors of the shared tunnel. Left out, or empty, it is
  every active node; a cluster of more than 25 nodes needs the list.
- `followMaintenance`: whether a node that Proxmox puts in maintenance is drained, and taken
  back when it leaves maintenance.
- `rotateOnForget`: whether forgetting a node rotates the secret of the shared tunnel, which
  the forgotten node's connector held. Off, the run token on the node's disk keeps working:
  the controller drops the connectors the node reported at Cloudflare, by their ids, and the
  node's agent stands down once it reads its removal, but a copy of the token taken elsewhere
  goes on working.
- `admissionMaxMoves`: how many routes may move to admit a node for routing. A node whose
  admission would move more stays joining until an admin admits it.

A key the file leaves out has its default, and is taken as written when it is there, as in
`managed`: `"admissionMaxMoves": 0` and `"agentNetwork": ""` are refused with their path. An
earlier release reads the settings strictly and refuses a file that has the object, so the
object is left out of the file while every key has its default: settings saved without a
cluster setting are a file that release reads. `pco settings show` shows `-` for it then.

## Egress filter

The egress filter has no setting in the file. Its two switches are decisions of the admin of
the node, kept in the local state of the node, where only root reads and writes them, and no
call of the API or of the web interface changes them. They are changed with `pco egress`, as
root:

| What | Command | File | Takes effect |
|---|---|---|---|
| Addresses the connectors never reach, whatever the daemon verified | `pco egress block <address>`, `pco egress unblock <address>` | `/var/lib/pco/egress-blocked.json` | At once; the daemon keeps the filter to it |
| The filter itself, off or on | `pco egress off`, `pco egress on` | `/var/lib/pco/egress-off.json`, present while the filter is off | At once |

While the filter is off the connectors are not confined; `pco status` and `pco doctor` say so
until it is on. See [Security](security.md) for what the filter stops.

## Web interface

The web interface has no setting in the file either. It is configured when `pco setup` makes
it, through the environment of its unit, and by the commands of its certificate:

| What | Where | Takes effect |
|---|---|---|
| The address it listens on | `PCO_WEB_LISTEN` in `/etc/default/pco-web`, written by `pco setup --web-listen <address>` | After `systemctl restart pco-web` |
| More host names it answers to | `PCO_WEB_HOSTS` | After `systemctl restart pco-web` |
| Strict-Transport-Security | `PCO_WEB_HSTS` | After `systemctl restart pco-web` |
| Whether `root@pam` may sign in with a password, in the appliance | `PCO_WEB_ALLOW_ROOT` | After `systemctl restart pco-web` |
| The certificate: `ca`, `own` or `pveproxy` | `pco setup --web-cert`, and `pco web cert import` and `renew` | See [Files](files.md) |

The variables are in the table of [Operations](operations.md#environment-variables).

## Hostname patterns

A pattern is `*`, a hostname, or `*.` followed by labels. `*.example.com` covers every name
below `example.com` at any depth, and not `example.com` itself. A wildcard route is denied
as well when a deny pattern names something below it: `secret.example.com` for
`*.example.com`, because the wildcard would serve that name too. Patterns and zone names are
lower-cased and checked when the file is read.

The apex of a zone, `example.com`, and a wildcard, `*.example.com`, are published for a guest
only when an `allowHosts` pattern names them: the apex by itself, a wildcard by a wildcard
pattern at or above it (`*.example.com` names `*.example.com` and `*.shop.example.com`). `*`,
and no `allowHosts` at all, name neither. Without that rule, whoever may edit the Notes of one
tagged guest could take the apex of every zone pco serves, or the wildcard that answers every
name of the zone that has no record of its own, with a valid certificate.

Such a route is in the state `rejected`, says which pattern to add, and is a problem line of
`pco status` and a warning of `pco doctor` (`rejected routes`). It takes no claim and holds
none, so that naming a hostname before you allow it wins nothing: once a pattern names it,
the hostname goes to whoever claims it then, as any free one does. A rejected route has no
rule and no record at Cloudflare; a record pco made for it before is kept and left alone.
Manual routes are root's own and are not limited by the patterns.

## A settings file the daemon cannot use

A file the daemon cannot use makes it hold, and `pco status` says why as a problem that
begins `reading the settings:`, until the file is fixed. What follows tells which kind of
mistake it is:

| The problem says | What is wrong |
|---|---|
| `reading the settings: <file>: invalid JSON at offset 170` | The file is not valid JSON: a stray comma, a missing bracket. The offset counts bytes from the start of the file. |
| `reading the settings: <file>: invalid: json: unknown field "denyhost"` | A key the daemon does not know, usually a misspelt one. Settings are read strictly, so that a misspelt `denyhost` cannot silently drop a list that was meant to deny. |
| `reading the settings: <file>: field "observeOnly" cannot be read as bool` | A value of the wrong type. |
| `reading the settings: <file>: invalid: time: invalid duration "ten"` | A duration that Go cannot read. |
| `reading the settings: stored settings are invalid: <field> ...` | A value that is not allowed: a gate tag that is no Proxmox tag, an `admission` or `identityMinimum` that is not one of its words, a prefix in `trustedCIDRs` or `manualCIDRs` that is not IPv4, a pattern or zone pin that does not normalise. The problem names the field. |
| `reading the settings: <file>: invalid: managed.<key> ...` | A key of `managed` that is empty, zero or out of its range, as `managed.detachGrace 0s: at least 1m0s`. It is checked as the file is read, where a key written zero can still be told from one left out. |
| `reading the settings: <file>: invalid: cluster.<key> ...` | A key of `cluster` that is empty, zero or out of its range, as `cluster.admissionMaxMoves 0: from 1 to 1000`, the same way. |
| `reading the settings: <file> has schema version 2, this build reads 1: ...` | The file was written by a newer version, which an older build never rewrites. |

A value below its minimum is no such mistake. A file that says `pollInterval` below 5
seconds, `grace` below 30 seconds or `maxHostnamesPerGuest` below 1 is read with the minimum in
its place, and `pco status` shows a problem for each, in this form:

```text
settings: pollInterval is 1s in /etc/pve/pco/meta/settings.json, below the minimum of 5s; 5s is used until it is raised there
```

The daemon does not rewrite the file. A command that stores the settings does, with the
raised values: `pco apply` when it leaves observe-only mode, `pco setup` when it makes a new
install or recovers one, and `pco settings apply`.

Every problem line of `pco status` is in [Problems](problems.md).
