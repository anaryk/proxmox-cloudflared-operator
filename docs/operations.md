# Operations

This page is the reference for running pco: the daemon and its units, where the state
lives, what the daemon does in each cycle and when it holds back, how to read its output
from a script, the settings, and upgrades.

## The daemon and its units

The package installs three systemd units.

| Unit | What it does |
|---|---|
| `pco.service` | The daemon, `pco daemon`. It is of type `notify`, restarts after 5 seconds when it stops, and has 150 seconds to start. It starts after `network-online.target`, `pve-cluster.service` and `pveproxy.service`, and before `pve-ha-lrm.service`. It does not wait for the guests, and they do not wait for it. |
| `pco-egress.service` | Loads the egress filter by running `pco egress load`. It is a one-shot unit that stays active, and it is ordered before `pco.service`. The connector units require it, so it starts with the first connector, at boot too. Setup does not enable it by itself. |
| `pco-cloudflared@<tunnel id>.service` | The connector of one tunnel. The daemon starts and stops these, one for each tunnel. They require `pco-egress.service`. |

The connectors keep running when the daemon stops, restarts or is upgraded. pco is never in
the data path: a request goes from Cloudflare's edge to a connector and from there to the
guest, and none of it passes through the daemon. If the daemon is down, published routes
keep working; what stops is everything that needs a decision: new routes, withdrawals, and
the removal of records. The egress table stays in the kernel as the daemon left it, with its
targets, but nothing loads it again if something removes it: `pco egress load` does.

The daemon waits for the cluster filesystem when it starts. Its first step is to read the
Proxmox token from `/etc/pve/priv/pco`, and if `/etc/pve` is not mounted yet it tries again
every 10 seconds for two minutes, and then gives up and exits, which systemd answers by
starting it again. It also takes an exclusive lock on `/var/lib/pco/daemon.lock`, so a
second daemon, whether started by hand or by systemd, refuses with `another pco daemon is
running on this node`.

A connector listens for metrics on `127.0.0.1`, on the lowest port from 20300 that no other
connector uses; the daemon reads its `/ready` endpoint to tell whether it is connected.

After a reboot the connectors start with an egress table that has no targets, so a published
hostname answers 502 until the daemon's first cycle has got a complete listing from Proxmox,
verified the routes and given the filter their targets (see [Security](security.md)). The
daemon starts after `pve-cluster.service` and `pveproxy.service`, and the addresses are verified
up to 32 at a time, so the time to the first serve grows with their number.

The daemon is not ordered before the start of the guests, so Proxmox may still be starting
them when the first cycles run. A guest that does not run yet is listed as stopped, and a
route that was served before the reboot is withdrawn with the reason `guest is not running`:
its hostname answers 503 and its DNS record stays. The route is served again in the first
cycle that finds the guest running and its address verified, which for the guests that start
last is some cycles after the daemon's first. A VM whose address comes from the guest agent
has it only once the agent runs in the guest.

## Where the state lives

The state is a set of small JSON files, one for each object, in three roots.

| Root | Holds | Shared |
|---|---|---|
| `/etc/pve/pco` | State that is not secret: the install, the settings, the writer identity, claims, approvals, the node registry. | On the cluster filesystem. |
| `/etc/pve/priv/pco` | Secrets: the Proxmox token and the Cloudflare credentials. | On the cluster filesystem, readable by root only. |
| `/var/lib/pco` | State of this node: the connectors' files, bindings, the event log, the lock. | No. |

The files:

| File | Content |
|---|---|
| `/etc/pve/pco/meta/install.json` | The install id (12 hexadecimal characters), when it was made, and the profile, `host`. |
| `/etc/pve/pco/meta/settings.json` | The settings; see below. |
| `/etc/pve/pco/meta/leader.json` | The writer identity: install id, generation and a nonce. The tunnel configuration carries it, and it is how pco tells its own writes from those of a stale or a foreign writer. |
| `/etc/pve/pco/meta/tombstones.json` | DNS records waiting out their grace period. |
| `/etc/pve/pco/nodes/<node>.json` | The node that runs pco, and the version. |
| `/etc/pve/pco/claims/<hostname>.json` | Who holds a hostname, since when, and who waits. A wildcard is stored as `_wildcard.<name>.json`. |
| `/etc/pve/pco/approvals/<owner>.json` | An approved guest and its identity. A guest `qemu/101` is stored as `qemu_101.json`. |
| `/etc/pve/pco/routes/<id>.json` | Manual routes, which the daemon reads in every cycle. No command writes them yet, and only root can. A manual route names an address and not a guest, so it is not proven (its level is `manual`), and its `allowNode` option lifts the rules of the denylist that keep the addresses of nodes out. See [Security](security.md). |
| `/etc/pve/pco/adopted.jsonl` | One line for each DNS record that `pco adopt` replaced, as it was; at most 256 KiB, oldest lines first out. |
| `/etc/pve/priv/pco/meta/pve-token.json` | The secret of the Proxmox token. |
| `/etc/pve/priv/pco/credentials/<id>.json` | One Cloudflare credential. |
| `/var/lib/pco/manifest.json` | What `pco setup` created; `pco uninstall` follows it. |
| `/var/lib/pco/bindings/<hostname>.json` | The address verified for each hostname, with the MAC, the level and, at `port`, where the forwarding table placed the MAC. A file is written when its binding changes, and for the time of the last proof alone only once that moved on by more than 75 seconds: after a restart a proof counts as up to that much older than it is. |
| `/var/lib/pco/meta/engine-memory.json` | What the daemon must still know after a restart: the zones it serves and every zone it ever served, the tunnels it saw, the guests you confirmed gone, the last check of each credential, and the targets of the egress filter with the tunnel configurations that confirmed them. |
| `/var/lib/pco/meta/node-addrs.json` | The addresses of the nodes, for the denylist. |
| `/var/lib/pco/events.log` | The event log, one JSON object a line; rotated at 5 MiB, one earlier file is kept. |
| `/var/lib/pco/tunnels/` | `<tunnel id>.token`, `.env` and `.yml` of each connector. The `.env` file names the install the connector belongs to. |
| `/var/lib/pco/egress-blocked.json`, `egress-off.json` | The block list and the off switch of the egress filter. |
| `/var/lib/pco/daemon.lock` | The lock of the node. |

Each object file wraps its data in an envelope with a schema version, a revision and the id
it was stored under:

    {
      "schemaVersion": 1,
      "rev": 1,
      "id": "settings",
      "data": { ... }
    }

The daemon refuses a file written by a newer schema version and a file that holds the object
of another id. It never creates the shared roots, only `pco setup` does, and it checks that
the cluster filesystem is mounted before it touches them: while `pve-cluster` restarts,
`/etc/pve` is an empty directory of the node's own disk, and what is read from it says
nothing. Do not edit these files by hand, except the settings.

## The cycle, in plain words

The daemon runs a cycle every `pollInterval` (10 seconds by default), and at once when you
ask: `pco sync`, `pco apply`, `pco adopt`, or when a credential is added. In each cycle it:

1. Reads the settings, the install, the node registry and the writer identity.
2. Asks Proxmox for every guest and reads the configuration of the tagged ones. Guests that
   are not tagged are read again only every five minutes. The addresses a guest reports are
   cached for a minute.
3. Reads the routes out of the Notes of the tagged guests and applies the hostname policy
   and, in approve mode, the approvals.
4. Settles the claims: which guest holds each hostname.
5. Verifies the address of each route that holds its hostname, up to 32 at a time, with
   15 seconds for each (see [Identity](identity.md)), and holds back what is below
   `identityMinimum`. Routes on the same address of a guest share one check. A proof the
   watch of the network vouches for is not made again until it is `reverifyInterval` old;
   see below.
6. Plans the state at Cloudflare: one tunnel for each account that holds a zone with routes,
   a rule for each route, a 503 rule for each hostname that is claimed and not served, and a
   proxied record for each hostname.
7. Gives the egress filter its targets: those it verified and those a confirmed tunnel
   configuration still uses, less the addresses that lost their proof (see
   [Security](security.md)). This comes before anything is written at Cloudflare.
8. Unless something holds the cycle, brings Cloudflare in line: creates the tunnel if it is
   missing, writes its configuration and reads it back, starts or restarts the connector,
   and creates or retargets DNS records, and removes the ones that are no longer wanted.
9. Publishes what it found, which is what `pco status` shows.

Two things run beside the cycle. A watch of the network reacts to a served address whose
MAC moves, without waiting for the next cycle. A keeper checks the egress table every 30
seconds and whenever nftables reports a change, and loads it again when it is gone or
changed. [Security](security.md) describes both.

### How often an address is checked on the wire

Checking an address takes the time of an ARP exchange, about 600 ms, which is what a cycle
of many routes waits for. So an address is checked once in a cycle however many routes
point at it, and a proof at `port` stands, in later cycles, for as long as the watch of the
network vouches for it. Each cycle still checks what needs no wire, the denylist, the
addresses of the nodes and the MACs of the other guests, and connects to the port. The
address is checked on the wire again in the next cycle when:

- the watch reported its MAC moved;
- the configuration of its guest changed, or the guest stopped, or moved to another node;
- its address comes due: once in every `reverifyInterval` (a minute by default), at a time
  each address takes from its own, so that the addresses proven in one cycle come due a
  share in each of the cycles that follow;
- the port did not answer, which has the address checked again in the same cycle;
- the proof is at `observed`, or placed the MAC on no bridge: the watch cannot see it move;
- the watch is not running: it could not start, the node is not Linux, or it started
  after the proof was made.

A new address is checked in two cycles in a row: the watch has it only after the first.
With the defaults and 1000 routes, each cycle checks about a sixth of the addresses on the
wire, which takes about 4 seconds; a cycle that checks every address, as one does while the
watch is not running, takes about 20 seconds.

### What Cloudflare's rate limit costs

Cloudflare allows a user 1200 requests in 5 minutes, whatever token they come with, and says
in every answer how many are left and when the count starts again. pco spends at most
`cloudflareBudget` of them for each credential, 1000 by default, which leaves 200 to the
dashboard and other tools: when Cloudflare says that fewer are left, pco keeps to that, and to
a lower limit Cloudflare names. Credentials of one Cloudflare user share the 1200, so give two
such credentials budgets that add up to no more.

A cycle reads each zone once, every record of it in one request up to 5000 records, and
decides from that listing which names to create, which to point at the tunnel again and which
are held by a record of someone else. A record costs one request to create; a change to one,
a removal and an adoption read the name again first.

A cycle does not wait for the budget for long. When the next request would wait longer than
20 seconds, the cycle writes nothing more, ends, and its state says how many changes wait,
in this form:

    11 changes wait for Cloudflare's rate limit

The cycles that follow make them once there are requests to spare. Until then their reads
are refused too, and they say so in one line,

    the tunnel of account <id> and the listing of zone <name> wait for Cloudflare's rate limit

and nothing is removed. The plan is the same in every cycle, so nothing is lost. An adoption
that replaces a record is begun only while there is room for its three requests: the delete,
the create, and the put-back should the create fail.

A first start of 1000 routes thus costs about 1015 requests: a few for the tunnel, a listing
of the zone and a create for each record. The first cycle makes about 990 of the records in a
few seconds, and the rest follow about 5 minutes later, when Cloudflare starts its count again.
An idle cycle costs three requests: the tunnel, its configuration and the listing of the
zone.

The zones and accounts of each credential are listed every five minutes, and each token is
checked again once a day. With the accounts, every five minutes, the daemon also reads the
run token of each tunnel again and lists the connectors Cloudflare shows on it: two calls
for each tunnel, on top of the two listings of each credential. A run token that changed,
as after its secret was rotated, is written to the connector's token file and the connector
is restarted with it. When a connector logs that Cloudflare refuses its token, the token is
read again at once, once for each refusal, and every 30 seconds while a read fails. While the
configuration of a tunnel is not yet seen running, or a connector that pco does not run is
shown on it, its connectors are listed every 30 seconds. The listing of the connectors goes
on in a cycle that holds, every five minutes; see [Security](security.md).

### Observe-only until `pco apply`

A new install, and one after `pco setup --recover`, is in observe-only mode. The daemon runs
every step up to the plan, shows what it would do (`pco plan`, where every action is held
by `observe mode`), and changes nothing at Cloudflare: no tunnel, no configuration, no
record, no connector. `pco apply` leaves the mode. To go back, set `observeOnly` to `true`
in the settings.

### Holding back

The rule of the daemon is that missing information is never taken for an empty answer. When
a step cannot be sure, the cycle holds: it changes nothing at Cloudflare and on the
connectors, `pco status` says why in its problems, and the next cycle tries again. The
daemon holds when:

- the settings, the install, the claims or the other state cannot be read, or the cluster
  filesystem is not mounted;
- the inventory of Proxmox is incomplete: a listing or a configuration could not be read, a
  guest is of an unknown kind, or the cycle was cut short. A partial inventory never leads
  to a removal;
- there is no Cloudflare credential, or a credential has not listed its zones since the
  daemon started (`credential <id>: its zones are not listed yet`): that holds the whole
  cycle, for every account, until its zones are listed or it is removed. It is what a
  token that was revoked or expired before a restart does, even if it is not the one that
  serves your zones (see [Cloudflare token](cloudflare-token.md));
- a zone is in doubt: seen through several credentials with no pin when none of them served
  it alone before, pinned to a credential that does not see it, gone from its credential's
  listing, or refused its DNS by the credential that serves it (see
  [Troubleshooting](troubleshooting.md#a-served-zone-whose-dns-is-refused)). That freezes the
  account of the zone, and not the whole cycle;
- there is no writer identity, or the writer is stale or foreign (see
  [Troubleshooting](troubleshooting.md));
- the guests that hold hostnames drop out of Proxmox's listing in numbers that look like a
  failure and not like their removal. Proxmox lists none of them, or more than five and
  more than 30 per cent are gone: this is what an API token that lost its privileges looks
  like, and the cycle holds until they are listed again or `pco apply --confirm-deletes`
  says they were removed on purpose;
- the egress filter cannot be given its targets, or its table cannot be loaded again after
  it was changed outside pco: the cycle then reads Cloudflare and writes no tunnel
  configuration until it can, because a rule must not send a connector to a target it
  cannot reach.

### Grace periods and the mass delete guard

pco removes a DNS record only after its hostname has been unwanted for the whole grace
period (`grace`, 60 seconds by default), which the daemon watches continuously: a stretch
in which it did not look, longer than two minutes, such as an outage of the daemon,
starts the grace again. Right before it deletes, it asks Proxmox once more whether any guest
still publishes the name. A claim is kept for the same grace when its holder stops asking
for it, so that a restart or a short edit of the Notes loses nothing, and then it goes to the
guest that has waited longest, or is released.

The mass delete guard holds deletes when many are pending at once. If more than 5 records
of this install are being removed, and more than 30 per cent of its records, none of them
is deleted until you confirm: `pco plan` lists what waits, and

    pco apply --confirm-deletes

shows it again and asks. What you confirm is exactly what was listed: the daemon refuses
when it changed in the meantime, and you look again. The same confirmation covers guests
that Proxmox no longer lists, zones that left their listing, and tunnels that no credential
sees. It needs a terminal, or `--yes` in a script.

Requests such as an adoption or a confirmation are one-shot: they wait for a run that can
make them, and expire after five minutes with an event.

## Events and logs

    pco events --since 10m

lists what changed, oldest first: routes that changed state, conflicts, what was applied at
Cloudflare, holds that began or ended, problems that appeared, what happened to the egress
filter, and what admins did. Each event has a time, a level (`info`, `warn`, `error`), a
kind (`route`, `conflict`, `action`, `problem`, `claim`, `rollout`, `writer`, `admin`,
`credential`, `hold`, `egress`), a subject and a message. `--since` takes a duration such as
`10m` or a time in RFC 3339 form. The daemon keeps the last thousand events since it
started; the journal has all of them, and the event log file keeps the recent ones.

The events of the kind `egress` say that the filter was switched off or on, that its table
was changed outside pco and loaded again, and that the MAC of a served address moved, with
what came of the verification that followed.

The daemon logs to the journal as JSON lines:

    journalctl -u pco

Events are logged too, with the field `event`. To change the level, give `pco daemon` the
flag `--log-level` (`trace`, `debug`, `info`, `warn` or `error`) in a drop-in of the unit.
`systemctl edit pco` opens one, and the lines to put in it are

    [Service]
    ExecStart=
    ExecStart=/usr/bin/pco daemon --log-level debug

The empty `ExecStart=` clears the one of the package; without it systemd refuses a second
command. Then run `systemctl restart pco`; the connectors keep running. They log to the
journal under their own units.

## Exit codes and `--json`

Every `pco` command that asks the daemon exits with one of three codes.

| Code | Meaning |
|---|---|
| 0 | All is well. |
| 1 | The command ran and found something to look at: problems in the state, or an egress filter that does not confine the connectors (`pco status`), a failed check (`pco doctor`), a failed step (`pco diagnose`), a filter that is off, not loaded or changed (`pco egress show`), a request the daemon refused, or any other failure. |
| 2 | The command could not ask the daemon: it is not running, its socket refused the connection, no answer came in time, or the answer could not be read. Run as root with the daemon not running, `pco doctor` makes the checks that need no daemon instead and exits by what it finds. |

`pco routes`, `pco plan`, `pco events`, `pco claims list` and `pco guest list` print what
they find and exit 0. `pco doctor` exits 1 for a failure and not for a warning, and
`pco diagnose` for a failed step and not for a skipped one.

With `--json`, the commands that print an answer of the daemon (`status`, `routes`, `plan`,
`events`, `claims list`, `guest list`, `diagnose`, `doctor`, `credential list`, `add` and
`check`) print it as the daemon sent it, re-indented, with control and bidirectional
characters escaped. On a command that has no answer to print, such as `setup` or `apply`,
`--json` is an error.

`pco routes --json` and `pco plan --json` print the whole state, the same document as
`pco status --json`, so that a script finds the fields in one place. The state has these
fields:

| Field | Content |
|---|---|
| `at`, `finishedAt` | When the last cycle began and ended. |
| `mode` | `observe` or `enforce`. |
| `complete` | Whether the inventory of Proxmox was complete. |
| `profile` | `host`. |
| `routes` | One object for each route: `hostname`, `owner`, `state`, `level`, `reason`, `service`, `zone`, `warnings`, `guest`, `candidates`. |
| `issues` | Problems in the Notes and the settings: `guest`, `line`, `col`, `msg`. |
| `tunnels`, `connectors` | The tunnels and the state of their connectors. `unchecked` on a tunnel says the last cycle did not look at Cloudflare, and `leftAsIs` that the tunnel is left as it is for a reason of its own, which `held` gives, as a frozen account. A connector has the `connectorId` its `/ready` gives, and `tokenRefused` or `metricsPortHeld` when its journal says Cloudflare refuses its token or another process holds its metrics port. |
| `rogueConnectors` | The connectors Cloudflare lists on a tunnel of the install that pco does not run: `tunnel`, `tunnelId`, `accountId`, `id`, `originIp`, `version`, `since`. |
| `admission`, `gateTagged` | The admission mode, and how many guests, templates included, carry the gate tag. |
| `credentials` | The credentials and the report of their last check. |
| `actions`, `conflicts`, `lost` | The pending actions, the records of someone else in the way, and the names that lost the marker. |
| `problems` | What the daemon found wrong, as text. |
| `waiting`, `offer` | What waits for a confirmation. |
| `unapproved` | In approve mode, the guests that wait. |
| `hold`, `writerVerdict` | Why the last cycle did not check Cloudflare, and what it found of the writer. |
| `egress` | The egress filter as the daemon last found it: `state` is `on`, `off`, `not loaded` or `changed`, and `since` says when the admin switched it off. Absent until the daemon has looked. |

A script should read the fields and not match the text. The messages in `problems`, `held`,
`reason` and `msg` are for people and can change; the field names, the route states and the
levels are the contract. A field that is added later is added next to the others and does not
change those that are there.

The route states are `active`, `unreachable`, `withdrawn`, `conflict`, `no-zone`, `held`,
`rejected` and `frozen`. `pco routes --state <state>` shows one of them.

## Settings

The settings are one file, `/etc/pve/pco/meta/settings.json`, which `pco setup` makes with
the defaults. There is no command to change them yet: edit the file. It is the object in
`data`; leave the rest of the envelope as it is. `python3 -m json.tool` checks the syntax.

A file the daemon cannot use makes it hold, and `pco status` says why as a problem that
begins `reading the settings:`, until the file is fixed. What follows tells which kind of
mistake it is:

| The problem says | What is wrong |
|---|---|
| `reading the settings: <file>: invalid JSON at offset 170` | The file is not valid JSON: a stray comma, a missing bracket. The offset counts bytes from the start of the file. |
| `reading the settings: <file>: invalid: json: unknown field "denyhost"` | A key the daemon does not know, usually a misspelt one. Settings are read strictly. |
| `reading the settings: <file>: field "observeOnly" cannot be read as bool` | A value of the wrong type. |
| `reading the settings: <file>: invalid: time: invalid duration "ten"` | A duration that Go cannot read. |
| `reading the settings: stored settings are invalid: <field> ...` | A value that is not allowed: a gate tag that is no Proxmox tag, an `admission` or `identityMinimum` that is not one of its words, a prefix in `trustedCIDRs` that is not IPv4, a pattern or zone pin that does not normalise. The problem names the field. |
| `reading the settings: <file> has schema version 2, this build reads 1: ...` | The file was written by a newer version, which an older build never rewrites. |

    {
      "schemaVersion": 1,
      "rev": 1,
      "id": "settings",
      "data": {
        "gateTag": "cf-tunnel",
        "denyHosts": ["*.internal.example.com"],
        "pollInterval": "10s",
        "grace": "1m0s",
        "admission": "approve",
        "observeOnly": false,
        "identityMinimum": "port"
      }
    }

Every field, with its default:

| Field | Default | What it is |
|---|---|---|
| `gateTag` | `cf-tunnel` | The tag that makes a guest a candidate. A Proxmox tag: lower-case letters, digits and `_ - + .`, not starting with `-`, `+` or `.`, at most 64 characters. Read when the daemon starts. After you change it, run `pco setup` again (or `pco setup --repair`), which registers the new tag besides `cf-tunnel` and `cf-tunnel-managed`; see [Security](security.md). |
| `allowHosts` | none | Patterns of the hostnames that may be published. Empty allows everything but the apex of a zone and a wildcard, which a guest publishes only when a pattern names them (below). |
| `denyHosts` | none | Patterns of the hostnames that may not be published. A deny rule wins over an allow rule. |
| `pollInterval` | `10s` | The time between two cycles. At least `5s`. |
| `grace` | `1m0s` | How long a removal waits (see above). At least `30s`. |
| `trustStatic` | `false` | Trust static addresses behind a router; see [Identity](identity.md). Read when the daemon starts. |
| `trustedCIDRs` | none | The IPv4 prefixes in which such addresses are trusted. Read when the daemon starts. |
| `admission` | `tag` | `tag` publishes a guest that carries the tag, `approve` also needs the approval of an admin. Use `approve` when anyone but the admins holds `VM.Clone` on a tagged guest or template: Proxmox copies the tag to the clone (see [Security](security.md)). |
| `zonePins` | none | A zone name and the id of the credential that serves it: `{ "example.com": "a1b2c3d4" }`. |
| `observeOnly` | `true` | Whether the daemon only observes. `pco apply` sets it to `false`. |
| `identityMinimum` | `port` | The lowest identity level that is served: `port`, `filtered` or `observed`. |
| `maxHostnamesPerGuest` | `32` | How many hostnames the Notes of one guest may name. A guest that names more publishes none of them, and keeps the ones it holds. At least `1`. |
| `reverifyInterval` | `1m0s` | How long a proof of identity that the watch of the network vouches for stands before the address is checked on the wire again. From `10s` to `5m0s`. |
| `cloudflareBudget` | `1000` | How many requests in 5 minutes pco spends of each credential, of the 1200 that Cloudflare allows a user; see below. From `100` to `1150`. Read when the daemon starts. |

Durations are written as Go reads them, `30s`, `90s`, `2m`, `1m30s`. A pattern is `*`, a
hostname, or `*.` followed by labels; `*.example.com` covers every name below `example.com`
at any depth, and not `example.com` itself. A wildcard route is denied as well when a deny
pattern names something below it, `secret.example.com` for `*.example.com`, because the
wildcard would serve that name too. Patterns and zone names are lower-cased and checked when
the file is read.

The apex of a zone, `example.com`, and a wildcard, `*.example.com`, are published for a
guest only when an `allowHosts` pattern names them: the apex by itself, a wildcard by a
wildcard pattern at or above it (`*.example.com` names `*.example.com` and
`*.shop.example.com`). `*`, and no `allowHosts` at all, name neither. Without that, whoever
may edit the Notes of one tagged guest could take the apex of every zone pco serves, or the
wildcard that answers every name of the zone that has no record of its own, with a valid
certificate. Such a route is in the state `rejected`, says which pattern to add, and is a
problem line of `pco status` and a warning of `pco doctor` (`rejected routes`). It takes no
claim and holds none, so that naming a hostname before you allow it wins nothing: once a
pattern names it, the hostname goes to whoever claims it then, as any free one does. A
rejected route has no rule and no record at Cloudflare; a record pco made for it before is
kept and left alone. Manual routes are root's own and are not limited.

The daemon reads the file again at the start of each cycle, so a change takes effect at the
next one. Four fields, `gateTag`, `trustStatic`, `trustedCIDRs` and `cloudflareBudget`, are
wired when the daemon starts. A change to those is noticed and shown as a problem,
`settings gateTag changed since pco started and are read only at start; restart pco
(systemctl restart pco) for them to take effect`, and applies after `systemctl restart pco`.

The minimums are guards. A grace of a moment would remove a record when one cycle happened
to miss its name, and cycles closer together would only ask Proxmox and Cloudflare more. A
file that says `pollInterval` below 5 seconds, `grace` below 30 seconds or
`maxHostnamesPerGuest` below 1 is read with the minimum in its place, and `pco status` shows a
problem for each, in this form:

    settings: pollInterval is 1s in /etc/pve/pco/meta/settings.json, below the minimum of 5s; 5s is used until it is raised there

The daemon does not rewrite the file. A command that stores the settings does, with the
raised values: `pco apply` when it leaves observe-only mode, and `pco setup` when it makes
a new install or recovers one. An unknown key is refused, so that a misspelt `denyhost`
cannot silently drop a list that was meant to deny.

### The rule for compatibility

A new setting is optional and has a safe default, so a settings file that was written
before it keeps working, and a field the file leaves out has its default. Settings are read
strictly, so a build that does not know a field refuses a file that has it. This means a
downgrade after a newer version added a field to the settings needs that field removed by
hand. The same holds for the schema version of every file: a file written by a newer schema
is refused and never rewritten. The envelope has version 1 and this is the only one that
exists.

## Upgrades

To upgrade, install the new package, with the installer or with `apt install` of the new
`.deb`; see [Quickstart](quickstart.md). On an upgrade the package reloads systemd and
restarts `pco.service` if it was running. The connectors are not touched, because
restarting them would drop the tunnels they serve; they run on with the binary they started
with, and the files of a connector are rewritten and the connector is restarted only when
the daemon finds them different from what it wants. A change that cloudflared does not read
is no reason to restart it: the env file of a connector from an earlier version, which lacks
the name of the install, is rewritten and the connector runs on.

From this release on, a guest's route for the apex of a zone or for a wildcard is published
only when an `allowHosts` pattern names it (see [Settings](#settings)), and a guest whose
Notes name more than `maxHostnamesPerGuest` hostnames, 32 by default, publishes none. Such
routes stop at the first cycle after the upgrade: they are `rejected`, their hostnames are no
longer in the tunnel's configuration, and the records pco made for them are kept and left
alone, so that they serve again once a pattern names them. Add the patterns before you
upgrade if those names are to go on.

`pco setup` can be run again after an upgrade: it changes nothing that is in order and notes
the new version in the node registry. `pco setup --repair` re-asserts the role, user, token
and tags in Proxmox, as after a restore of the node or an upgrade of Proxmox VE that changes
privileges.

The package does not touch the egress table either. When a release changes the filter, the
table in place is not the one the new daemon loads: `pco egress show` and `pco status` say so
until the daemon's next check loads the new one, and `pco egress load` does it by hand.
Downgrades are not supported: see the rule for compatibility above.

`cloudflared` comes from Cloudflare's apt repository, and `apt upgrade` updates it. A
running connector keeps the binary it started with until it restarts. To use the new one,
restart the connectors, one at a time. A connector that stops gives its open requests up to
30 seconds to finish (`--grace-period 30s`, and the unit allows 45 seconds to stop), and the
routes of its tunnel are unreachable for a short moment while it reconnects:

    systemctl restart pco-cloudflared@<tunnel id>

`pco doctor` warns when `cloudflared` is more than ten months old.

## One node

pco runs on one node. `pco setup` refuses on a second node of a cluster
(`pco is already set up on node <name>; cluster support arrives in a later release`), and the
daemon holds when the node registry names another node. On a cluster, install it on the node
whose guests you want to publish. The guests of the other nodes are listed by Proxmox and
their routes are read, but they can only be proven at `observed`; see [Identity](identity.md).

## For packagers

The binary is built with the Go build tag `nomsgpack`, which the Makefile, the lint
configuration, the release configuration and the tests all pass. Without it the HTTP library
links a msgpack codec the daemon's API never uses, about 6 MB of binary. A build without it
works.
