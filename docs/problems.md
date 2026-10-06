# Problems

This page lists what pco says when something is wrong, with what each line means, what
causes it and what to do: the problem lines of `pco status`, the kinds of events of `pco
events`, the checks of `pco doctor`, and the words for a route that is not served, a change
that is held and what waits for a confirmation. [Troubleshooting](troubleshooting.md) is the
narrative for the cases that come up most, and links here for the rest.

## Reading a line

A problem line is a sentence for people. `pco status` lists the problems of the last cycle,
the web interface shows them on its overview, and `pco events` shows each one once, as an event
of the kind `problem`, when it appears. `pco doctor` has a check, `problems`, that fails while
there is one. A script reads the fields of `pco status --json` and not the text: the lines
can change, and what holds them is the contract (see [Operations](operations.md)).

In the lines below, a name in angle brackets stands for what the daemon puts there: an id, a
name, an error from the system or from Cloudflare. A line that says `nothing is changed` or
`the rest is left as it is` belongs to a cycle that **holds**. The rule of the daemon is
that missing information is never taken for an empty answer: when a step cannot be sure, the
cycle changes nothing at Cloudflare and on the connectors, says why in the problems, and the
next cycle tries again. The first reason a cycle gave is its hold, which `pco status --json`
has as `hold` and which becomes the event `the cycle holds: <reason>`; every tunnel then
shows as `unchecked`. [Operations](operations.md#holding-back) lists when the daemon holds.

An id of a tunnel, an account or a credential is written in full in the lines. Examples
here use visibly made-up ones.

## The state and the node

| Line | Meaning and cause | What to do |
|---|---|---|
| `pco is not set up on this node; run pco setup` | The store has no install, or the local root `/var/lib/pco` is missing. The cycle holds. | `pco setup`, or `pco setup --recover` after a lost store. |
| `cluster filesystem is not mounted` | `/etc/pve/.version` is missing: `/etc/pve` is not the mounted cluster filesystem, as while `pve-cluster` starts or restarts. What is read from an empty directory says nothing, so the cycle holds. | `systemctl status pve-cluster`. The daemon goes on by itself once it is mounted. |
| `<doing>: <error>` | The store could not be read, or an object in it is not valid: a file that is not JSON, one of a newer schema version, one that holds another id, or a disk that fails. `<doing>` is `reading the settings`, `reading the install identity`, `reading the node registry`, `reading the writer identity`, `reading the claims`, `reading the acknowledged segments`, `reading the credentials`, `reading the manual routes`, `reading the approvals`, or `reading the soft deny list`. The cycle holds. | The error names the file; see [Files](files.md). Fix it or restore it: the daemon never takes a file it cannot read for an empty one. |
| `reading the saved node addresses: <error>` | `meta/node-addrs.json` cannot be read. The cycle holds. | As above; the daemon makes the file again from what Proxmox lists once it is removed. |
| `reading the bindings: <error>` | A file in `/var/lib/pco/bindings` cannot be read. The cycle holds. | The error names the file; see [Files](files.md). |
| `reading what the engine remembered: <error>; nothing is changed at Cloudflare until it can be read: fix the file or remove it; removing it forgets the connectors kept for tunnels no credential sees, the zones that left their listing and the guests confirmed gone` | `meta/engine-memory.json` is unreadable. The cycle holds. | Fix the file, or remove it and accept what the line says it forgets. |
| `the engine memory on this node is of install <id>, not <id>; it is set aside and replaced` | After a new setup on a node, the memory of an earlier install is found. It is not used. Shown once. | Nothing; look at [a connector of another install](#tunnels-and-connectors) if connectors of the earlier one still run. |
| `saving what the engine remembers: <error>; nothing is changed at Cloudflare until it is saved` | `meta/engine-memory.json` cannot be written: the disk is full or read-only. | Free the disk. |
| `saving the claims: <error>; nothing is changed at Cloudflare until they are saved` | The claims cannot be written to `/etc/pve/pco/claims`: the cluster filesystem is read-only, as without quorum, or full. | `systemctl status pve-cluster`; `pvecm status`. |
| `saving the bindings: <error>`, `saving the node addresses: <error>`, `saving the soft deny list: <error>`, `saving the MACs pinned to the claims: <error>` | A write of the state failed. The cycle goes on, and tries the write again in the next one. | Look at the error; usually the same cause as above. |
| `node <name> is not registered; run pco setup` | The node registry does not name this node. The cycle holds. | `pco setup`. |
| `the node registry names <names> besides <node>; pco runs on one registered node only` | More than one node is registered. Cluster support comes in a later release. The cycle holds. | Install pco on one node of the cluster; see [Operations](operations.md#one-node). |
| `building the denylist: <error>` | The addresses pco never serves could not be put together. The cycle holds. | Report it with the journal; it is a defect. |
| `the cycle ended <where> (<error>); the rest is left as it is` | The cycle was cut short, as by a stop of the daemon, at `before the tunnels`, `before the connectors` or `before DNS`. | Nothing: the next cycle does the rest. |
| `the cycle ended while addresses were resolved (<error>); nothing is changed` | The same, while addresses were verified. | Nothing. |

### Settings and overrides

| Line | Meaning and cause | What to do |
|---|---|---|
| `reading the settings: <file>: <error>` | The settings file cannot be used; the forms of the error are in [Settings](settings.md#a-settings-file-the-daemon-cannot-use). The cycle holds. | Fix the file. |
| `settings: <name> is <value> in <file>, below the minimum of <minimum>; <minimum> is used until it is raised there` | `pollInterval`, `grace` or `maxHostnamesPerGuest` is below its minimum; the minimum is used. | Raise it, with `pco settings apply`. |
| `settings <names> changed since pco started and are read only at start; restart pco (systemctl restart pco) for them to take effect` | `gateTag`, `trustStatic`, `trustedCIDRs` or `cloudflareBudget` was saved, and the daemon has the old value. | `systemctl restart pco`. The connectors keep running. |
| `the Cloudflare API is overridden to <url> (PCO_CLOUDFLARE_API_URL); this is for tests only` | The daemon runs with `PCO_CLOUDFLARE_API_URL`, which points it at a fake Cloudflare on this machine. | A production node never has it: `systemctl cat pco` shows where it is set; remove it and `systemctl restart pco`. |
| `the release host or key is overridden (PCO_UPGRADE_BASE, PCO_UPGRADE_KEYRING); this is for tests only` | The daemon has one of those variables. | The same. |

## The writer

The writer is the identity that is written into the configuration of every tunnel, and that
`leader.json` names. A daemon writes only while the two agree. See [the writer is stale,
foreign or unknown](troubleshooting.md#the-writer-is-stale-foreign-or-unknown).

| Line | Meaning and cause | What to do |
|---|---|---|
| `no writer identity; run pco setup` | `leader.json` is missing. The cycle holds. | `pco setup`. |
| `the writer identity in leader.json is not valid: <error>` | The file has no valid identity. The cycle holds. | `pco setup --recover`. |
| `leader.json names install <id>, but this is install <id>; run <command>` | The store was set up anew and `leader.json` is of another install. The command is `pco setup --recover`, or `pco appliance recover` in the appliance. The cycle holds. | The command. |
| `the writer identity cannot be read <when> (<error>); the rest is left as it is`, `leader.json names another writer <when>; the rest is left as it is` | `<when>` is `after the tunnel run` or `after the DNS run`: the identity could not be read, or another one is stored, by the time the run ended. The cycle holds. | If another daemon of this install writes, stop it; otherwise `pco setup --recover`. |
| `<reason>; this writer is stale and stops`, with the prefix `<tunnel> in account <id>:`, `dns:` or `<name> in zone <zone>:` | A newer generation of this install wrote, or the identity changed during the run. `<reason>` is `leader.json names generation <n> nonce <nonce>, not this writer (generation <n> nonce <nonce>)`, `the writer identity changed during the run`, or `the configuration was written by generation <n> nonce <nonce>, newer than this writer`. The run writes nothing more. | `pco setup --recover` on the node that should write. |
| `reading the writer identity: <error>`, `cannot write as this writer: <error>`, with the prefix `<tunnel> in account <id>:` before a write, `dns:` at the start of the DNS run, or none at the start of the tunnel run | The identity could not be read, or it does not pass validation. The run does not write. | Fix `leader.json`, or `pco setup --recover`. |
| `<name> in zone <zone>: <reason>; writing stops` | The check made again right before a change to a record found the identity unreadable. | As above. |
| `<tunnel> in account <id>: the configuration was written by generation <n> nonce <nonce>, which leader.json does not know: <hint>; writing stops` | A writer that `leader.json` does not know wrote the tunnel's configuration: another install with this id, a sentinel written with a stolen token, or a store that was lost. The hint names the command: `pco setup --recover`, or in an appliance `pco appliance recover`. | See [Security](security.md) for the order: replace the token, `pco tunnel rotate`, then recover. |
| `dns: the writer is of install <id>, the settings are for install "<id>"` | The writer and the settings disagree about the install. The DNS run does not start. | `pco setup --recover`. |
| `dns: the tunnel run of this cycle found a <stale writer or foreign writer>; changing no dns record` | The DNS run does not write after the tunnel run found a writer problem. | As for the writer. |
| `<tunnel> in account <id>: the plan is not for the tunnel of this install, <name>`, `... the planned rules do not end with the catch-all`, `... the planned rules lack the sentinel of this writer before the catch-all` | The plan the tunnel run was given is not one it may write. Nothing is written for that account. | A defect; report it with the journal. |

## Proxmox and the inventory

The inventory is what the daemon read from Proxmox. A cycle works on a **complete** one only.
A partial inventory never leads to a removal. A guest is written as `qemu/101`, or `lxc/102`,
with its name in parentheses where it has one.

| Line | Meaning and cause | What to do |
|---|---|---|
| `the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are` | A listing or a configuration could not be read, a guest is of an unknown kind, or the cycle was cut short. The cycle holds; the lines below say what was not readable. | Fix what they say. `pco doctor` shows the API of Proxmox in `proxmox`. |
| `cluster resources not listed: <error>` | The Proxmox API did not list the guests: it does not answer, or the token of pco has lost its privileges. | `pco setup --repair` re-asserts the role, user and token; `pco doctor`. |
| `cluster nodes not listed: <error>`, `node <node>: network not refreshed: <error>` | The nodes, or the network of one, could not be read. The cycle holds. | As above. |
| `refresh cancelled: <error>` | The read was cut short, as by a stop of the daemon. | Nothing: the next cycle reads again. |
| `guest <ref> is listed on both <node> and <node>` | Proxmox lists one guest on two nodes, as for a moment during a migration. The cycle holds. | Wait; if it stays, look at the cluster. |
| `guest <guest>: building guest <vmid>: unknown guest kind "<kind>"` | Proxmox listed a guest that is neither a virtual machine nor a container. The cycle holds. | Report the kind; pco does not know it. |
| `guest <guest>: config not refreshed: <error>`, `config for <guest> not found on <node>; will re-check` | The configuration of a guest could not be read. A guest that just moved to another node answers not found too, which is not taken for its removal. The cycle holds. | Usually passes in the next cycle. |
| `guest <guest>: node <node> is offline and the guest is not cached` | A guest with routes is on a node that is offline and the daemon has nothing cached for it. The cycle holds. | Bring the node back. |
| `node <node> is offline; using cached data for its guests` | The data of its guests is the last read. Not a hold. | Bring the node back. |
| `status of <n> guests is unknown; using last known state` | Proxmox gave no status for those guests. Not a hold. | Usually passes. |
| `reported addresses unavailable: the API token lacks the guest-agent privilege (VM.GuestAgent.Audit, VM.Monitor before PVE 9)` | pco cannot ask the guests for the addresses they report. A VM whose address comes from the guest agent has none. Not a hold. | `pco setup --repair` gives the role the privilege. |
| `guest <guest>: interfaces not refreshed: <error>` | The agent of a guest, or a container, did not answer. Not a hold. | See whether the agent runs in the guest. |
| `Proxmox lists no guest at all, but <n> guests hold a hostname (<guests>); nothing is changed: check the privileges of the Proxmox API token, or run pco apply --confirm-deletes if they were removed on purpose` | The vanish guard: a complete listing is empty while guests hold hostnames. The cycle holds. | `pco setup --repair`, and `pco doctor`; `pco apply --confirm-deletes` if they were removed on purpose. |
| `<n> of <m> guests that hold a hostname are no longer listed by Proxmox (<guests>); nothing is changed until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose` | The vanish guard: more than 5 of the guests that hold a hostname, and more than 30 per cent of them, are gone from the listing at once. This is what a token that lost its privileges looks like. The cycle holds. | As above. |

## Hostnames, claims and routes

| Line | Meaning and cause | What to do |
|---|---|---|
| `settings contain an invalid allow or deny pattern; nothing is changed until it is fixed` | A pattern in `allowHosts` or `denyHosts` does not normalise. The cycle holds. | Fix the pattern; see [Settings](settings.md#hostname-patterns). |
| `hostname <hostname> of <owner> is not published: <reason>` | The hostname policy refuses a route that is the apex of a zone or a wildcard, which a guest publishes only when an `allowHosts` pattern names it. `<reason>` is `the apex of zone <zone> is published only when allowHosts names it: add "<hostname>" to allowHosts`, or `a wildcard is published only when an allowHosts pattern names it: add "<hostname>" to allowHosts`. The route is `rejected`, takes no claim and has no rule or record. | Add the pattern, or take the name out of the Notes. |
| `<n> routes are held back: their identity level is <level>, below the required <level>; guests on other nodes and trusted static addresses are proven at observed only: lower identityMinimum in the settings to serve them` | `identityMinimum` is above what the proof of those routes reaches; they answer 503 and have no record. For one route the line says `1 route is held back: its identity level is`. | See [a route is held back by the identity minimum](troubleshooting.md#a-route-is-held-back-by-the-identity-minimum). |
| `<n> routes are held by the observed rules: <causes>; pco routes says why each is held` | Routes served at `observed` wait for an acknowledgement or an approval. The line counts them by cause: `segment not acknowledged`, `MAC changed` (the MAC of the guest is not the one approved), `delegated guest` (principals hold `VM.Config.Network` on it), `access control unreadable`, and `soft-denied address` (the address is a gateway or a resolver of a node). For one route it says `1 route is held by the observed rules`. | `pco routes` says which; `pco segment acknowledge <bridge>`, `pco guest approve <owner>`; see [Security](security.md). |
| `the access control of Proxmox has not been read yet; every guest's observed routes wait for approval` | Only while `identityMinimum` is `observed`: the daemon has not yet read who may do what in Proxmox. | Passes with the next read; if it stays, see the next line. |
| `the access control of Proxmox could not be read since <time>; every guest's observed routes wait for approval: <error>` | The same, and the read keeps failing. | Check the privileges of the Proxmox token with `pco setup --repair`. |
| `reading the SDN subnets or the resolvers of the nodes: <error>; the ones read before stay denied` | The addresses that must never be served could not be refreshed; the earlier ones are kept. | Look at the error. |
| `reading the gateway and resolvers of the appliance: <error>; the ones seen before stay soft-denied` | The appliance could not read its own gateway or resolvers. | See [the appliance](#the-appliance). |

A guest whose Notes name more than `maxHostnamesPerGuest` hostnames has no route at all, and
an issue says so; issues are not problem lines (see [Annotations](annotations.md)).

## Credentials and zones

| Line | Meaning and cause | What to do |
|---|---|---|
| `no Cloudflare credential; add one with pco credential add` | There is no credential. The cycle holds. | See [Cloudflare token](cloudflare-token.md). |
| `credential <id> (<label>): its last check found that the token cannot be used: <checks>; once it is fixed, pco credential check <id> says so` | The last check of the token failed a capability, as a permission that is gone. | Grant what `<checks>` names, then `pco credential check <id>`. |
| `credential <id>: building its client: <error>` | The Cloudflare client for the credential cannot be made. | Look at the error. |
| `credential <id>: its zones are not listed yet (<why>); nothing is changed at Cloudflare until they are` | The token has not listed its zones since the daemon started: it lacks `Zone > Zone > Read`, was revoked or expired, or Cloudflare did not answer. This holds the cycle for every account, until the zones are listed or the credential is removed. | `pco credential check <id>`; `pco credential remove <id>` for a dead token. |
| `credential <id>: listing its zones failed (<error>); using the list from <time>` | The last listing failed and the earlier one is used. Not a hold. | Passes when Cloudflare answers. |
| `zone <name> is pinned to credential <id>, which can list it but not read its DNS; it is not served until the pin is changed or the credential is granted Zone > DNS > Edit on it` | `zonePins` names a credential that cannot read the zone's records. | Grant `Zone > DNS > Edit`, or change the pin. |
| `zone <name> is pinned to credential <id>, which does not see it; <account> is left as it is until the pin is fixed` | The pin names a credential that does not list the zone. The account is frozen. | Fix the pin in `zonePins`. |
| `zone <name> is visible through credentials <ids>; pin it with zonePins (<id> serves it until then)` | Several credentials see a zone; one that served it before keeps it. | Pin it. |
| `zone <name> is visible through credentials <ids> and none of them served it before; pin it with zonePins; <account> is left as it is until then` | The same, with no earlier server: the account is frozen. | Pin it. |
| `zone <name> is no longer listed by credential <id>; <account> is left as it is until the zone is listed again or pco apply --confirm-deletes confirms it is gone` | A zone that was served left the listing of its credential. The account is frozen. | `pco apply --confirm-deletes` if it was removed on purpose; otherwise see why the token lost it. |
| `the token of credential <id> could not read the DNS of zone <name>, which it serves; <account> is left as it is, checking again at <time>`, `credential <id> can no longer read the DNS of zone <name>, which it serves: grant it Zone > DNS > Edit there; ...`, `zone <name> is listed by credential <id>, whose last check did not look at it; ...` | The three steps of a zone whose DNS is refused. | See [a served zone whose DNS is refused](troubleshooting.md#a-served-zone-whose-dns-is-refused). |

## Tunnels and connectors

| Line | Meaning and cause | What to do |
|---|---|---|
| `<tunnel> in account <id>: no client for credential <id>` | The tunnel run has no Cloudflare client for the credential. | Look at the credentials with `pco credential list`. |
| `<tunnel> in account <id>: finding the tunnel: <error>`, `... creating the tunnel: <error>`, `... finding the tunnel after its name was taken: <error>`, `... the name is taken, but no tunnel of that name is found` | A call to Cloudflare for the tunnel failed or answered something that does not add up. | Look at the error; Cloudflare may not have answered. The next cycle tries again. |
| `<tunnel> in account <id>: reading the configuration: <error>`, `... reading the configuration again: <error>`, `... reading the configuration back: <error>` | The tunnel's configuration could not be read. | As above. |
| `<tunnel> in account <id>: writing the configuration: <error>` | The write was refused. The tunnel shows `VERIFIED no`. | Look at the error; a token that cannot edit tunnels is the usual cause. |
| `config changed under us on <tunnel> in account <id>` | The configuration read back after a write is not what was written: someone changed it at Cloudflare in the meantime. | If it recurs, see who else writes to the tunnel. |
| `tunnel <tunnel> in account <id> serves no zone pco sees; the tunnel and its connector are left as they are until a credential lists a zone of the account or the tunnel is deleted at Cloudflare` | The account has a tunnel of this install and no zone that pco serves. | List a zone of the account through a credential, or delete the tunnel at Cloudflare. |
| `tunnel <tunnel> in account <id> is not visible through any credential; its connector is kept until a credential sees the account again or pco apply --confirm-deletes confirms the tunnel is gone` | No credential sees the account of a tunnel that was served. The connector is kept. | `pco apply --confirm-deletes` if the tunnel is gone. |
| `tunnel <tunnel> in account <id>: reading the connector status: <error>`, `connector for tunnel <id>: reading its status: <error>`, `listing the connectors on this node: <error>` | The unit of a connector could not be asked. | `systemctl status` of the unit. |
| `tunnel <tunnel> in account <id>: reading the connector token: <error>`, `... fetching its token: <error>`, `... reading its token again: <reason>; its connector keeps the one it has`, `... starting its connector: <error>` | The files of a connector could not be read, written or started. | The error names the file or the unit; `journalctl -u pco-cloudflared@<tunnel id>`. |
| `tunnel <tunnel> in account <id>: Cloudflare refuses the token its connector runs with; pco reads the token again when that starts and every five minutes, and pco tunnel rotate gives the tunnel a new secret` | The connector's journal says Cloudflare refuses its run token, as after the secret was rotated. | Wait for the read; or `pco tunnel rotate --account <id>`. |
| `tunnel <tunnel> in account <id>: reading its token again failed: <reason>; it is read again at <time>, while Cloudflare refuses the token its connector runs with` | The reread did not work. | As above. |
| `tunnel <tunnel> in account <id>: metrics port <port> is held by another process, which keeps its connector from starting; <remedy>` | Another process listens on the metrics port the connector was given. The remedy says when it gets another. | Nothing; find the process if it recurs. |
| `tunnel <tunnel> in account <id> is served by connector <connector>, which pco does not run on this node: it takes a share of the requests to every hostname of the tunnel; unless you run it, rotate the tunnel secret with pco tunnel rotate --account <id>` | Cloudflare lists a connector on the tunnel that is not one of this node's. Whoever runs it gets a share of the requests. | If it is not yours, `pco tunnel rotate --account <id>`. |
| `connector for tunnel <id> names no install; <command> adopts the install it was made for, pco uninstall on this node removes it` | A connector, as one from before pco wrote the install into its env file. The daemon never stops it. | `pco setup --recover`, or `pco uninstall`; see [Troubleshooting](troubleshooting.md#a-connector-of-another-install). |
| `connector for tunnel <id> belongs to install <id>; <command> adopts that install, pco uninstall on this node removes it` | A connector of another install runs on this node. | The same. |
| `connectors are not pruned in this cycle: <reasons>` | The daemon removes the connector of a tunnel that is gone only once it knows every account was looked at and answered. The reasons say which was not. | Nothing: the next cycle tries again. |
| `removing the connectors of other tunnels: <error>` | Removing the connector of a tunnel that is gone failed. | `systemctl` shows the unit. |

## DNS records

| Line | Meaning and cause | What to do |
|---|---|---|
| `zone <name>: no client for credential <id>`, `zone <name>: listing the records: <error>` | The records of a zone could not be listed. The zone is left alone. | Look at the error and `pco credential check <id>`. |
| `<name> in zone <zone>: planned <n> times (<plans>); not writing it` | Two plans want one name. | A defect; report it. |
| `<name> in zone <zone>: creating the record: <error>`, `... updating the record: <error>`, `... deleting the record: <error>` | Cloudflare refused the change. The action shows the error as its held reason in `pco plan`. | Look at the error; for a refusal of permission, `pco credential check <id>`. |
| `<name> in zone <zone>: reading the record again before changing it: <error>`, `... reading the record again before deleting it: <error>`, `... looking up the records of that name: <error>`, `... asking the inventory before deleting: <error>` | A check made right before a change failed, so the change is not made. | Passes when the call does. |
| `<name> in zone <zone>: a record of this install appeared during the run; trying again on the next one` | A record with the marker of this install was found where the run was about to create one. | Nothing. |
| `<name> in zone <zone>: storing the record before the adoption: <error>` | The copy of the record that `pco adopt` replaces could not be written to `adopted.jsonl`, so nothing is replaced. | Look at the error; see [Files](files.md). |
| `<name> in zone <zone>: adoption failed; the original record was put back` | The CNAME could not be created after the record was deleted; the record is back. | Look at why the create failed. |
| `<name> in zone <zone>: the record was removed for the adoption; the current writer creates the CNAME on its next run; the original was <record>` | The run stopped as the writer changed, with the name empty. | Nothing; the writer that runs next creates it. |
| `<name> in zone <zone>: adoption failed and the original record is gone; restore it by hand: <record>` | The create failed and the record could not be put back. The line carries the whole record. | Create it again by hand with what the line says. |
| `mass delete guard: <n> of <m> records are being removed; confirm to proceed` | More than 5 records of this install, and more than 30 per cent of them, are to be removed. No delete is made. A count in parentheses says how many are in zones that could not be listed. | `pco plan` lists them; `pco apply --confirm-deletes`. |
| `loading dns tombstones: <error>; changing no dns record` | The record of names that wait out their grace period, `meta/tombstones.json`, cannot be read. The run changes no record. | See [Files](files.md). |
| `saving dns tombstones: <error>` | It cannot be written. The run deletes no record, held as `tombstones not saved`. | The same. |
| `account <id>: listing probe tunnels: <error>`, `<tunnel>: listing its connectors: <error>`, `<tunnel>: deleting the probe tunnel: <error>`, `<name> in zone <zone>: reading the probe again before deleting it: <error>` | The sweep of the test tunnels and records that a credential check leaves behind failed. | Passes; they are swept again. |
| `run stopped: <error>` | The tunnel run was cut short. | Nothing. |

## Cloudflare's rate limit

| Line | Meaning and cause | What to do |
|---|---|---|
| `<n> changes wait for Cloudflare's rate limit` | The credential spent its `cloudflareBudget`, or Cloudflare says that fewer requests are left. The cycle writes no more and ends. The plan is the same in every cycle, so nothing is lost. | Nothing; see [Operations](operations.md#what-cloudflares-rate-limit-costs). |
| `<reads and changes> wait for Cloudflare's rate limit` | The same, and not even a read is left: `the tunnel of account <id> and the listing of zone <name> wait for Cloudflare's rate limit`. Nothing is removed meanwhile. | The same. |

## The egress filter and the service prefix

| Line | Meaning and cause | What to do |
|---|---|---|
| `the egress table was changed or removed outside pco and was loaded again` | The nftables ruleset was changed, as by `nft flush ruleset` or `nftables.service` starting. The daemon put its table back; the line carries what differed. | Find what changes the ruleset; `pco doctor` has the check `nftables`. |
| `the egress table was changed or removed outside pco and could not be loaded again: <error>; no tunnel configuration is written until it is` | The same, and it could not be put back. The daemon writes no tunnel configuration while the connectors are not confined. | `pco egress show`, then `pco egress load`; the journal says why it failed. |
| `checking the egress table: <error>` | The table could not be listed. | `pco egress show`. |
| `setting the egress filter: <error>; no tunnel configuration is written until it is set` | The cycle could not give the filter its targets, and a rule must not send a connector to a target it cannot reach. | `pco egress show`; the journal. |
| `taking <address> out of the egress filter: <error>` | An address that lost its proof could not be removed from the table. | `pco egress show`. |
| `the service-prefix route or table was changed outside pco and was loaded again` | The appliance's route or table of the service prefix was changed, and the daemon put it back. | Find what changed it. |
| `the service-prefix route or table was changed outside pco and could not be loaded again: <error>` | The same, and it could not be put back. This holds no tunnel run: the egress table still confines the connectors. | `pco net show`, then `pco net load`. |
| `checking the service-prefix route and table: <error>` | They could not be read. | `pco net show`. |

## The appliance

These lines come from a daemon in the appliance, where the container proves in each cycle
that it is the one that was installed, and that no principal of Proxmox can reach into it.
A container that is a copy, and one that a principal can reach into, stop serving: the
identity flag goes, the egress filter is emptied and the connectors are stopped.

| Line | Meaning and cause | What to do |
|---|---|---|
| `the install of this appliance records no appliance; nothing is changed` | The install record has no appliance block. The cycle holds. | `pco appliance recover`. |
| `the volume at /var/lib/pco is <volume>, a volume of VMID <n> and not of <guest>: this container is a copy; the connectors are stopped and the egress filter is empty` | The state volume belongs to another container: this one is a copy. | Do not run the copy. Remove it, or recover it on purpose. |
| `Proxmox lists no <guest>: this container is not the one installed; the connectors are stopped and the egress filter is empty` | No container of the VMID it was installed as exists. | The same. |
| `<reason>; the connectors are stopped and the egress filter is empty` | Nothing proves that this container is the installed one: its state volume has no form pco knows and another guest carries its MAC, Proxmox reports no uptime of it, or its uptime differs from the one Proxmox reports. The reason names the facts. | `pco appliance repair` on the node, if it is the appliance. |
| `<principal> holds <privileges> on the appliance lxc/<vmid>; pco serves nothing while it does: <commands>` | A principal other than an admin can reach into the appliance. The connectors stop while it can. | The line gives the `pveum acl modify ... --roles NoAccess` commands. |
| `the access control of Proxmox has not been read since pco started; the connectors start once it shows that no principal can reach into the appliance` | After a start, the connectors wait for the first read of the access control. | Nothing. |
| `the inventory is incomplete; self-identification waits for a complete one`, `the uptimes of the guests could not be read; self-identification waits for them`, `the facts of this container could not be read: <error>` | The check cannot be made yet. Writes are held until it can. | Passes; if not, look at the error. |
| `the links of this container carry <MACs>, but <guest> in Proxmox has <MACs> for more than 60 s; writes are held until they match` | A network card of the container and its configuration in Proxmox disagree. | `pco appliance repair`, or put the configuration right in Proxmox. |
| `<guests> in pool pco carries a MAC of <guest>: a copy of the appliance runs; writes are held until it is gone` | A guest of the pool of the appliance carries its MAC. | Remove the copy. |
| `the certificate of <address> no longer verifies under <name> (the cluster CA or the pveproxy certificate changed?): run pco appliance repair --vmid <n> on the node` | The appliance cannot verify the Proxmox API any more. | The command the line gives. |
| `this container is <guest>, but its writer epoch could not be decided: <error>` | The identity is proven and the epoch could not be saved. | Look at the error. |
| `a NIC of the appliance <guest> appeared or vanished within the last minute; its links and its configuration in Proxmox must match by then` | A network card of the container and its configuration do not agree yet. Not a hold. | Passes within a minute. |
| `writing the identity flag: <error>; the connectors do not start until it is written`, `removing the identity flag: <error>`, `emptying the egress filter: <error>`, `stopping the connectors: <error>` | A step of starting or stopping the connectors failed. | Look at the error. |
| `the cluster is not quorate; nothing is written` | Without quorum the cluster filesystem is read-only, so nothing could be saved. The cycle holds. | Restore the quorum of the cluster. |
| `the quorum of the cluster could not be read (<error>); nothing is written` | The same, when the status could not be read. | Look at the error. |
| `the state of this appliance is older than its last write at Cloudflare (rollback or restore); run pco appliance recover` | The volume was rolled back or restored, so the writer behind it is older than the one at Cloudflare. The cycle holds. | `pco appliance recover`. |
| `leader.json belongs to an earlier start of this container; a new epoch is drawn once self-identification passes` | The container started again; the writer gets a new epoch after the identity is proven. The cycle holds meanwhile. | Nothing. |

## Event kinds

`pco events` lists what changed, oldest first. Each event has a time, a level (`info`, `warn`
or `error`), one of these kinds, a subject and a message. The daemon keeps the last thousand
since it started; the journal has all of them.

| Kind | Level | Reports | What to do |
|---|---|---|---|
| `route` | `info` when the route is `active`, else `warn` | A route changed state: `<owner>: <state> (<reason>)`, or `<owner>: no longer routed`. The subject is the hostname. | See [Route states](#route-states). |
| `conflict` | `warn` when it appears, `info` when it clears | A record of someone else stands in the way: `<type> <content> in zone <zone> is not ours; the hostname is not published`, and `... no longer conflicts`. | `pco adopt <hostname>`; see [Troubleshooting](troubleshooting.md#records-in-the-way-and-pco-adopt). |
| `action` | `info` | A change was made at Cloudflare: `create-tunnel`, `put-config`, `create-record`, `update-record`, `delete-record`, or `delete-tunnel`, which only ever removes a test tunnel a credential check left behind, with what it was about. | Nothing. |
| `problem` | `warn` | A problem line appeared; the message is the line. | See the lines above. |
| `claim` | `info`, `warn` for a conflict | What happened to the claim on a hostname: `claimed`, `identity-changed`, `transferred`, `released` or `conflict`, with the owner and the reason; also that a MAC was pinned to a claim at `observed`. | `pco claims list`. |
| `rollout` | `info` | The connectors run a configuration: `configuration version <n> runs on <m> connectors in account <id>`. | Nothing. |
| `writer` | `error`, `warn`, `info` | The writer verdict changed (`writer verdict is <verdict>`, see [Writer verdicts](#writer-verdicts)), or the appliance drew a new epoch after a start of the container. The subject is `leader.json`. | See [the writer](#the-writer). |
| `admin` | `info`, `warn` when a request expired | What an admin did, from the command line or the web interface: leaving observe-only mode, confirmations, settings saved, approvals and their revocation, credentials added and removed, adoptions requested and applied, claims moved, manual routes, segments acknowledged, a tunnel secret rotated, a restart of the daemon. The actor is who asked. | Nothing. |
| `credential` | `warn` | A token expires soon (`the token of credential "<label>" expires at <time>, in <n> days; ...`), could not be checked, or was refused the DNS of a zone it serves. | Add a new token with `pco credential add`, then remove the old one; see [Cloudflare token](cloudflare-token.md). |
| `hold` | `warn` when it begins, `info` when it ends | The cycle began or stopped holding: `the cycle holds: <reason>`, `the cycle no longer holds`. | The reason is a problem line. |
| `egress` | `warn`, `info`, `error` | The filter was switched off or on, its table was changed outside pco and loaded again or not, or the MAC of a served address moved and what came of the verification that followed. | See [a connector cannot reach its target](troubleshooting.md#a-connector-cannot-reach-its-target). |
| `connector` | `error` for a connector that is not yours, else `info` | A connector pco does not run was found on a tunnel of the install, or is gone; or the run token of a tunnel changed at Cloudflare and its connector restarts. | For a connector that is not yours, `pco tunnel rotate --account <id>`. |
| `identity` | `error`, `warn`, `info` | The appliance found itself a copy or no longer does, guests carry a MAC of the appliance, or principals can reach into it. The subject is the container. | See [the appliance](#the-appliance). |
| `web` | `info` | What the daemon did for the web interface: `web certificate renewed: <why>, fingerprint <fingerprint>`, when it made a new key and certificate of the cluster CA because the old one ends within 30 days, another CA signed it, it lacks a name or address the node has now, or `tls.crt` is a link. The subject is `web certificate`. | Nothing. |
| `net` | `warn`, `error` | What `pco-net.service` keeps in the appliance was changed and loaded again, or could not be. The subject is `service prefix`. | `pco net show`. |

## Doctor checks

`pco doctor` prints a line for each check: `✓` for fine, `!` for a warning and `✗` for a
failure, with a fix when it is not fine. Only a failure makes the exit status 1. The checks
that read the state say `not known until the first cycle` before there is one. These checks
run in every profile; [the last part](#checks-of-the-appliance) lists the ones that run only
in the appliance, and the rows for `store`, `writer` and `nftables` say how they differ there.

| Check | Fails or warns when | Fix |
|---|---|---|
| `cycle` | Warns when no cycle has run yet, or the last one ended more than three poll intervals ago (three times its own duration, if it took longer than a poll interval); fails after six. | `journalctl -u pco` says what holds the cycles up. |
| `cloudflared` | It does not run (fails), did not answer in time or has no version that can be read (warns), or is more than ten months old (warns; the age counts from the first day of the month in its version). | Install or update `cloudflared` from the package repository of Cloudflare. |
| `outbound` | TCP to `region1.v2.argotunnel.com:7844` cannot be made. A warning when every connector is connected all the same, over QUIC perhaps; else a failure. | Allow outbound TCP and UDP to port 7844. |
| `proxmox` | The Proxmox API does not answer with the token of pco (fails), the version cannot be told (warns), or it is older than 8.4 (fails). | Check the token with `pco setup --repair`; upgrade Proxmox VE. |
| `store` | The store is not mounted (fails) or not set up (fails). | `systemctl status pve-cluster`; `pco setup`. In the appliance: `pco appliance repair --vmid <vmid>` on the node. |
| `node lock` | This daemon does not hold the lock of the node (fails). | `systemctl restart pco`, so that no second daemon can start. |
| `egress` | The filter is switched off, its table is not loaded or not the one pco loads (fails: the connectors are not confined); the daemon has not checked yet, or found it in a state it does not know (warns). Without a daemon it also fails when the connector user does not exist. | `pco egress on`, or `pco egress show` and `pco egress load`. |
| `nftables` | `nftables.service` is enabled (warns): when it starts or restarts, its ruleset flushes the egress table. In the appliance the unit is expected to be masked: enabled fails, because its ruleset flushes both tables of pco; any state but masked or not installed warns; so does an answer that systemd does not give. | `systemctl disable nftables.service`, or keep its rules from flushing the whole ruleset. In the appliance: `systemctl mask nftables.service`. |
| `mode` | Observe-only mode (warns). | `pco apply`. |
| `inventory` | The inventory is incomplete (fails). | `pco status` lists the problems. |
| `writer` | The verdict is `stale`, `foreign` or `unknown` (fails); in the appliance also `behind`, the state older than the last write at Cloudflare. | See [Writer verdicts](#writer-verdicts). The command differs by profile: `pco setup --recover` on a host, `pco appliance recover` in the appliance, and `pct exec <vmid> -- pco appliance recover` from the node for `behind`. |
| `problems` | The last cycle found a problem (fails), with the first one. | `pco status`. |
| `conflicts` | A record of someone else stands in the way (warns). | `pco adopt <name>`. |
| `lost markers` | A record that points at the tunnel lost the marker of this install (warns). | `pco adopt <name>`. |
| `waiting` | Something waits for a confirmation (warns). | `pco apply --confirm-deletes`. |
| `rogue connectors` | Cloudflare lists a connector that pco does not run on this node on a tunnel of the install (fails). | `pco tunnel rotate --account <id>`, unless you run it. |
| `admission` | Admission is `tag`, guests carry the gate tag, and pco cannot tell who may clone one (warns), or no cycle has read the guests yet (warns). | Set `admission` to `approve`, unless only admins hold `VM.Clone` on the tagged guests. |
| `rejected routes` | The hostname policy refuses a route (warns). | Add the pattern to `allowHosts`, or take the name out of the Notes. |
| `credentials` | There is no credential (fails). | `pco credential add --label <label>`. |
| `credential <id>` | A token is not checked yet, could not be checked, cannot be used or has expired (fails for the last two), or expires in less than 30 days (warns). | `pco credential check <id>`, or add a new token and remove this one. |
| `approval`, `approval <owner>` | A guest waits for approval (warns, one finding for each). | `pco guest approve <owner>`. |
| `tunnel <name> in account <id>` | A tunnel is not checked, left as it is, unknown, does not exist yet or is not verified (warns). | `pco status` lists the problems; `pco plan`. |
| `connector <name> in account <id>` | The unit does not run or is not connected (fails), or it runs and whether it is connected was not checked (warns). | `systemctl status` and `journalctl -u` of the unit. |
| `web certificate` | Only where setup made the web interface: the certificate cannot be read, does not match its key or has expired (fails); it ends in less than 30 days (warns, in the modes `ca` and `own`). | `pco web cert renew`, or `pco web cert import <crt> <key>` for a certificate of your own, or `pco setup --repair` for the mode `pveproxy`. |
| `unit <unit>`, `daemon` | Only while the daemon does not answer, as root: the unit `pco.service` or `pco-egress.service` does not run (fails) or does not start at boot (warns); `daemon` warns that the checks that need the daemon were not made. | `systemctl enable --now <unit>`, and `journalctl -u <unit>`. |
| `doctor` | The run of the checks stopped on a defect (fails). | `journalctl -u pco` shows where it stopped. |

### Checks of the appliance

These run only in an appliance, where the daemon knows its container, its volume, its network
and its place in Proxmox. A check that cannot read what it looks at warns, `<what> could not
be read: <error>`, with the fix `the api and proxmox checks say why Proxmox does not answer`;
the rows below do not repeat that. `<vmid>` is the number of the container, and a fix that
says `on the node` is run on the Proxmox node and not in the appliance.

| Check | Fails or warns when | Fix |
|---|---|---|
| `access` | The access control of Proxmox cannot be read (fails: no connector starts until it is, and nothing says that no principal can reach into the appliance), or a principal other than an admin and pco's own holds privileges on the appliance (fails: the connectors are stopped meanwhile). | `pveum acl modify <path> --users <user> --roles NoAccess`, or `--tokens` for a token that separates its privileges, as the line gives it; for an unreadable access control, check that the token holds role PCO on `/` (`pveum acl list`). |
| `api` | The certificate of the Proxmox API no longer verifies under the server name it was installed with, as when the cluster CA or the `pveproxy` certificate changed (fails), or the API does not answer (fails). | `pco appliance repair --vmid <vmid>` on the node; or check that `pveproxy` runs and that the appliance reaches port 8006. |
| `clock` | The clock is 30 seconds or more away from the `Date` of Cloudflare's answer (warns), or 5 minutes or more (fails); or Cloudflare does not answer (warns). | The container shares the clock of the node: check its time synchronisation, chrony or `systemd-timesyncd`. |
| `cloudflare api` | `api.cloudflare.com` does not answer for pco (warns). Every change at Cloudflare needs it. | Let the appliance reach `api.cloudflare.com` on port 443. |
| `cmode` | The console mode is not `tty`, now or at the next start (fails): the console is then a root shell without a password, and any one `VM.Config` privilege sets it. | On the node: `pct set <vmid> --cmode tty`, or `pct set <vmid> --revert cmode` for a pending value, and restart the container. |
| `disk` | `/` or `/var/lib/pco` is 80 per cent full (warns) or 90 per cent (fails), or its use cannot be read (warns). | `apt-get clean`; on the node `pct resize <vmid> rootfs +1G` or `pct resize <vmid> mp0 +1G`. |
| `dns` | No name server is configured or the file cannot be read, or the name servers do not answer for `region1.v2.argotunnel.com` (fails): the connectors cannot resolve the edge. | On the node: `pct set <vmid> --nameserver <address>`, then restart the container; `pct config <vmid>` shows what it was given. |
| `egress probe` | As the user `pco-connector`, which the egress filter confines, `region1.v2.argotunnel.com:7844` does not connect, or the address of the Proxmox API connects or fails in another way than refused (fails); the install records no address of the API to probe (warns). | For the edge, a way out to Cloudflare on port 7844, TCP and UDP; for the API, `pco status` says why and `pco egress show` shows the table. |
| `epoch` | Never: it has a finding only when this daemon drew a new epoch after a start of the container, and says so. After a rollback, approvals, acknowledged segments and credentials made after the snapshot are gone. | Nothing. |
| `etc-pve` | `/etc/pve` exists in the container (fails): a bind mount of the cluster filesystem, which with the node's `www-data` group mapped in lets the container read the TLS keys of the node. | On the node: `pct set <vmid> --delete <mpN>` for the mount point whose `mp` is `/etc/pve`. |
| `features` | The features of the container are not exactly `nesting=1`, now or in the pending configuration (fails). | On the node: `pct set <vmid> --features nesting=1`, or `pct set <vmid> --revert features`, and restart the container. |
| `firewall` | Never; it says whether the datacenter firewall and the firewall of the node are on or off. It warns only when they cannot be read. | Nothing. |
| `holds` | `pco` or `cloudflared` is not held by apt (warns): apt or `unattended-upgrades` may replace it. | `apt-mark hold pco cloudflared`. |
| `identity` | No cycle has run, or no self-identification has (warns); the container is a copy, or the self-identification did not pass (fails). | For a copy, stop it; a restored container becomes the appliance with `pco appliance repair --vmid <vmid>` on the node, once the original is gone. Otherwise see [the appliance](#the-appliance). |
| `journal` | The journal takes 64 MiB or more (warns), where the appliance keeps it to 64 MiB. | `journalctl --vacuum-size=32M`, and look in `journalctl -p warning` for what writes so much. |
| `memory` | Tasks waited for memory in more than 10 per cent of the last 10 seconds (warns). | On the node: `pct set <vmid> --memory <megabytes>`; the line suggests twice the current value. |
| `net` | What `pco-net.service` loads is not in place (fails); packets were sent to the service prefix without a mapping and rejected since the table was loaded, or they cannot be counted (warns). | `pco net show`. The daemon loads the table again within 30 seconds. A few packets after a route was withdrawn are usual; a count that grows is not. |
| `network grants` | pco's user or its token may put cards on a whole zone, every bridge or vnet of it, present and future, or on a bridge that carries an address of the node (warns). | On the node: `pco appliance revoke-network --vmid <vmid> --bridge <bridge>` (with `--vlan`), `pveum acl delete` for a whole zone, then `pco appliance grant-network` one network at a time. |
| `protection` | The container is not protected against removal (fails): one command can destroy it with the volume of its state. | On the node: `pct set <vmid> --protection 1`. |
| `quic buffer` | `net.core.rmem_max` is below 7500000, the receive buffer `cloudflared` asks for its QUIC connections (warns). It is a setting of the node, which a container cannot change. | If you want it, on the node: `echo net.core.rmem_max=7500000 >> /etc/sysctl.d/90-pco.conf`, then `sysctl --system`. |
| `segment access` | A principal other than an admin and pco's own may use `SDN.Use` on the segment of the appliance's first card (warns): a guest put on it with the MAC of the appliance cuts off its inbound traffic. It also warns when the card or the access control is not known. | Put the appliance on a segment only admins may use, or take `SDN.Use` from the principals the line names. |
| `snapshots` | The container has a snapshot, bar one named `pco-pre-upgrade-<date>` that is less than 7 days old, or a replication job copies it (warns): each carries a copy of the state volume, with the Cloudflare credentials and the keys. | On the node: `pct delsnapshot <vmid> <name>`, `pvesr delete <job>`. |
| `token` | The token of pco lacks privileges of role PCO on this release of Proxmox VE, or its permissions cannot be read (fails); it holds more than the role gives (warns). | On the node: `pveum role modify PCO --privs <privileges>`, and grant it on `/` to the user and the token, as the line gives it. |
| `unattended-upgrades` | It is not enabled and configured (warns): the Debian security updates are not installed on their own. | `systemctl enable unattended-upgrades.service`, and keep `APT::Periodic::Unattended-Upgrade "1"` in `/etc/apt/apt.conf.d`. |
| `versions` | It says what runs: pco, `cloudflared` and Debian. `cloudflared` is more than ten months old, or the list of vetted versions cannot be read or is more than 90 days old (warns); the list denies the installed `cloudflared` (fails). | `pco upgrade cloudflared`; `pco upgrade --check` says whether a newer release is out. |
| `volume` | `/var/lib/pco` is not a mount of its own with the volume marker (fails), or `mp0` is missing, is mounted elsewhere, or is not kept out of backups (fails): a backup would carry the credentials and the keys. | `pco appliance repair --vmid <vmid>` on the node; for the backup, `pct set <vmid> --mp0 <the value with backup=0>` on the node. |
| `web listen` | `pco-web` listens on an address other than net0's (warns): its other cards are legs into the networks of guests, which could then reach the sign-in page and try the passwords of the cluster there. It also warns when net0's address or the listen address cannot be read, or none is set, as `pco-web` then refuses to start. | Set `PCO_WEB_LISTEN=<net0's address>:8643` in `/etc/default/pco-web`, then `systemctl restart pco-web`. When net0's address is not known, write it into the file the warning names and restart `pco-web`. |

## Route states

A route is in one of these states, which `pco routes --state <state>` filters by. The note
beside it, its reason, is for people; the state is the contract.

| State | Meaning | Look at |
|---|---|---|
| `active` | Served: its address is verified and the tunnel has a rule and a record for it. | Nothing. |
| `unreachable` | Not served: no address could be verified, or the port does not answer. A route that was never served gets a 503 rule and no record; one that was served keeps both, and visitors get a 502. | [A route is unreachable](troubleshooting.md#a-route-is-unreachable) has the reasons. |
| `withdrawn` | Was served, and lost its identity or the guest stopped. It answers 503 and its record stays. | `pco routes` for the reason; it is served again when the check passes. |
| `conflict` | Another guest holds the hostname. | `pco claims list`; `pco claims resolve <hostname> <owner>`. |
| `no-zone` | No credential serves the zone of the hostname, or two do and no pin chooses, or the hostname is reserved. | See the reasons below. |
| `held` | The hostname is claimed and nobody serves it, so that no other owner's wildcard serves it meanwhile. It answers 503. | The reason names the holder. |
| `rejected` | The hostname policy publishes the name only when `allowHosts` names it. No claim, rule or record. | Add the pattern, or take the name out of the Notes. |
| `frozen` | The account of the zone is left as it is, because a zone is in doubt. | The note says which; see [Credentials and zones](#credentials-and-zones). |

The reasons the plan gives, which `pco routes` shows in the note:

| Reason | Meaning |
|---|---|
| `no Cloudflare zone for this hostname in any credential` | No token sees an active zone that holds the name. |
| `zone <name> is visible through several credentials; pin it to one` | Pin it in `zonePins`. |
| `zone <name> is served through no credential` | The zone holds the name and no credential serves it. |
| `zone <name> is not served yet` | The zone is one this install never served, so the route takes no claim until it is; the guests that name the hostname then get it as any free one. |
| `reserved hostname` | The name is in the domain where the sentinels of the writer live. |
| `no verified address yet` | The address of the guest is not verified. |
| `target is not answering` | The address is verified and the port does not answer. |
| `identity check failed` | A served address lost its proof. |
| `address must never be served` | The address may never be published; the reason that comes with it names why, as `address of this node`. |
| `address was verified for another owner` | What resolution knows of the address is about another owner. |
| `identity level <level> is below the required <level>` | See [Identity](identity.md). |
| `hostname is held by <owner>` | The holder is another guest; the route is in `conflict`. |
| `named in the Notes of <owner> but not routed; claim kept` | The holder still names the hostname without a route, so it is `held`. |
| `no longer claimed by <owner> since <time>; released after the grace period` | The holder no longer asks for it, so it is `held` until the grace ends. |
| `a wildcard is published only when an allowHosts pattern names it: add "<hostname>" to allowHosts`, `the apex of zone <zone> is published only when allowHosts names it: add "<hostname>" to allowHosts` | The route is `rejected`. |

The reasons of an address that is `unreachable`, such as `no ARP answer on vmbr0`, are in
[Troubleshooting](troubleshooting.md#a-route-is-unreachable).

## Why a change is held

An action of `pco plan` has a held reason when it is not made in this cycle. The reasons:

| Reason | Meaning |
|---|---|
| `observe mode` | The daemon is in observe-only mode; `pco apply` leaves it. |
| `mass delete guard: <n> of <m> records are being removed; confirm to proceed` | See [DNS records](#dns-records); the reason begins with `mass delete guard`. |
| `grace period: <time> left` | A record of a hostname that is no longer wanted waits out `grace`; the reason begins with `grace period`. |
| `Cloudflare's rate limit` | The credential's budget is spent; a later cycle makes it. See [Cloudflare's rate limit](#cloudflares-rate-limit). |
| `tombstones not saved` | The record of names waiting out their grace could not be saved, so no record is deleted in this run. |
| `rate limit: next write in <time>` | The configuration of a tunnel is written at most once in 15 seconds. |
| `tunnel not created yet`, `tunnel state unknown`, `tunnel configuration not verified` | A record waits for its tunnel: it does not exist, its state is unknown, or its configuration is not read back yet. |
| `inventory incomplete` | A delete or an adoption waits for a complete inventory. |
| `snapshot not stored` | An adoption could not first store the record it replaces. |
| `writer changed`, `writer unreadable` | The run stopped as the writer changed or could not be read. |
| `no inventory confirmation`, `inventory confirmation failed` | Right before it deletes, the daemon asks Proxmox whether a guest still publishes the name. The first says the run had no way to ask, the second that asking failed. |

## What waits for a confirmation

`pco plan` lists what `pco apply --confirm-deletes` would accept, and it is in the state as
`waiting`. What you confirm is exactly what was listed.

| Kind | What it is |
|---|---|
| `dns-removals` | DNS removals held by the mass delete guard. |
| `vanished-guests` | Guests that Proxmox no longer lists, behind a vanish hold. A confirmation takes them as removed. |
| `stale-zone` | A zone that left the listing of its credential, or whose DNS it can no longer read. |
| `unseen-tunnel` | A tunnel that no credential sees. A confirmation removes its connector. |

## Writer verdicts

`pco status` shows what the last cycle found of the writer, and `pco doctor` has the check
`writer`.

| Verdict | Meaning | What to do |
|---|---|---|
| `ok` | This daemon writes the tunnel configuration. | Nothing. |
| `stale` | A newer generation of this install writes it. | `pco setup --recover` on the node that should write. |
| `foreign` | A writer that `leader.json` does not know wrote it: another installation with this install's id, or a sentinel written with a stolen token. | See [Security](security.md); otherwise stop the other installation. |
| `unknown` | `leader.json` could not be read or used. | `pco setup`, or `pco setup --recover`. |
| `behind` | The state of the appliance is older than the last write at Cloudflare. | `pco appliance recover`. |
