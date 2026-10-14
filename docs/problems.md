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
| `building the denylist: <error>` | The addresses pco never serves could not be put together. The cycle holds. | Report it with the journal; it is a defect. |
| `the cycle ended <where> (<error>); the rest is left as it is` | The cycle was cut short, as by a stop of the daemon, at `before the tunnels`, `before the connectors` or `before DNS`. | Nothing: the next cycle does the rest. |
| `the cycle ended while addresses were resolved (<error>); nothing is changed` | The same, while addresses were verified. | Nothing. |
| `the agent of this node does not take the desired state of this cycle: <error>; nothing is changed at Cloudflare, and the egress set, the gateway and the connectors are left as they are` | The agent that runs the connectors and the egress filter on this node checked what the cycle would ask of it before anything changed, and would refuse it: the desired state of a controller older than the one it follows, as after the store was restored from an earlier copy, or one whose order number goes back. Such a controller must change nothing; the routes are still resolved, so that the state shows how they fare, and an address that lost its proof still leaves the egress filter. | When the store was restored from an earlier copy, bring it forward before anything is changed. The next cycle tries again. |
| `the agent of this node did not take the desired state of this cycle after its tunnel run: <error>; DNS and further changes are held, and the connectors are left as they run` | The agent could not keep on disk what the cycle ended with, or refused it then. The tunnel configurations were written already, to targets the egress filter was given in the same cycle; DNS is not written. The connectors keep running as they were, with the tokens they have, and their statuses are read. | When the error names a file of the node's own state, as `desired.json` under `/var/lib/pco/agent`, fix the disk or its permissions. The next cycle tries again. |

### Settings and overrides

| Line | Meaning and cause | What to do |
|---|---|---|
| `reading the settings: <file>: <error>` | The settings file cannot be used; the forms of the error are in [Settings](settings.md#a-settings-file-the-daemon-cannot-use). The cycle holds. | Fix the file. |
| `settings: <name> is <value> in <file>, below the minimum of <minimum>; <minimum> is used until it is raised there` | `pollInterval`, `grace` or `maxHostnamesPerGuest` is below its minimum; the minimum is used. | Raise it, with `pco settings apply`. |
| `settings <names> changed since pco started and are read only at start; restart pco (systemctl restart pco) for them to take effect` | `gateTag`, `trustStatic`, `trustedCIDRs` or `cloudflareBudget` was saved, and the daemon has the old value. | `systemctl restart pco`. The connectors keep running. |
| `the Cloudflare API is overridden to <url> (PCO_CLOUDFLARE_API_URL); this is for tests only` | The daemon runs with `PCO_CLOUDFLARE_API_URL`, which points it at a fake Cloudflare on this machine. | A production node never has it: `systemctl cat pco` shows where it is set; remove it and `systemctl restart pco`. |
| `the release host or key is overridden (PCO_UPGRADE_BASE, PCO_UPGRADE_KEYRING); this is for tests only` | The daemon has one of those variables. | The same. |
| `the test hooks are on (PCO_TEST_HOOKS); this is for tests only` | The daemon runs with `PCO_TEST_HOOKS=1`: a cycle holds as it begins while `/run/pco/hold-cycle` exists, which the test suites use to see the watchdog restart a daemon that makes no progress. | The same: remove the variable and `systemctl restart pco`. |
| `finish the upgrade first (pco cluster finish-upgrade): the previous release would refuse the settings file` | A settings save that sets a field of the `cluster` block is refused while an upgrade from an earlier release is not finished: that release reads the settings file strictly, and a node that goes back to it would refuse the file and not start. | Finish the upgrade once every node runs this release, then save the setting again. |

## The writer

The writer is the identity that is written into the configuration of every tunnel, and that
`leader.json` names. A daemon writes only while the two agree. See [the writer is stale,
foreign or unknown](troubleshooting.md#the-writer-is-stale-foreign-or-unknown).

| Line | Meaning and cause | What to do |
|---|---|---|
| `no writer identity; run pco setup`, in the appliance `no writer identity; pco appliance recover draws one, with a Cloudflare credential stored` | `leader.json` is missing. The cycle holds. | `pco setup`. In the appliance `pco appliance recover`, after `pco credential add` when no credential is stored. |
| `the writer identity in leader.json is not valid: <error>` | The file has no valid identity. The cycle holds. | `pco setup --recover`. |
| `leader.json names install <id>, but this is install <id>; run <command>` | The store was set up anew and `leader.json` is of another install. The command is `pco setup --recover`, or `pco appliance recover` in the appliance. The cycle holds. | The command. |
| `the writer identity cannot be read <when> (<error>); the rest is left as it is`, `leader.json names another writer <when>; the rest is left as it is` | `<when>` is `after the tunnel run` or `after the DNS run`: the identity could not be read, or another one is stored, by the time the run ended. The cycle holds. | If another daemon of this install writes, stop it; otherwise `pco setup --recover`. |
| `<reason>; this writer is stale and stops`, with the prefix `<tunnel> in account <id>:`, `dns:` or `<name> in zone <zone>:` | A newer generation of this install wrote, or the identity changed during the run. `<reason>` is `leader.json names generation <n> nonce <nonce>, not this writer (generation <n> nonce <nonce>)`, `the writer identity changed during the run`, or `the configuration was written by generation <n> nonce <nonce>, newer than this writer`. The run writes nothing more. | `pco setup --recover` on the node that should write. |
| `reading the writer identity: <error>`, `cannot write as this writer: <error>`, with the prefix `<tunnel> in account <id>:` before a write, `dns:` at the start of the DNS run, or none at the start of the tunnel run | The identity could not be read, or it does not pass validation. The run does not write. | Fix `leader.json`, or `pco setup --recover`. |
| `<name> in zone <zone>: <reason>; writing stops` | The check made again right before a change to a record found the identity unreadable. | As above. |
| `<tunnel> in account <id>: the configuration was written by generation <n> nonce <nonce>, which leader.json does not know: <hint>; writing stops` | A writer that `leader.json` does not know wrote the tunnel's configuration: another install with this id, a sentinel written with a stolen token, or a store that was lost. The hint names the command: `pco setup --recover`, or in an appliance `pco appliance recover`. | See [Security](security.md) for the order: replace the token, `pco tunnel rotate`, then recover. |
| `<tunnel> in account <id>: the configuration was written by install <id>, generation <n>, not by this install: <hint>; writing stops` | The configuration carries the sentinel of another install. pco never writes over what another install wrote. | Find the other install: one tunnel name belongs to one install, so the configuration was copied or edited by hand. See [Security](security.md). |
| `<tunnel> in account <id>: the configuration holds a sentinel pco cannot read: <hint>; writing stops` | A rule of the configuration names a host in the `.invalid` domain, where only sentinels live, that does not read as one: an edited or forged configuration. It counts as another writer, never as no sentinel. | Look at the configuration in the dashboard; see [Security](security.md). |
| `<tunnel> in account <id>: the configuration holds no sentinel of this install; left as it is` | A tunnel pinned to a node, or one this install has no plan for, holds rules that this install did not write: pco leaves it alone. | Delete or rename the tunnel at Cloudflare if it is not needed. |
| `dns: the writer is of install <id>, the settings are for install "<id>"` | The writer and the settings disagree about the install. The DNS run does not start. | `pco setup --recover`. |
| `dns: the tunnel run of this cycle found a <stale writer or foreign writer>; changing no dns record` | The DNS run does not write after the tunnel run found a writer problem. | As for the writer. |
| `<tunnel> in account <id>: the plan is not for the tunnel of this install, <name>`, `... the planned rules do not end with the catch-all`, `... the planned rules lack the sentinel of this writer before the catch-all` | The plan the tunnel run was given is not one it may write. Nothing is written for that account. | A defect; report it with the journal. |

## The lease

On the host, the node that holds the lease, the lock `/etc/pve/priv/lock/pco-leader` in the
cluster filesystem, is the only one that writes: to the store, to Cloudflare and to Proxmox. A
node that stands alone holds it too, as the cluster filesystem runs in local mode there. Each
time a node takes it anew, `leader.json` gets a new epoch, one generation higher: once per boot
on a node that stands alone, and on a node of a cluster at every start after a clean stop,
which gives the lease back. A restart in the same boot that did not give it back keeps it. See
[Operations](operations.md#the-lease).

| Line | Meaning and cause | What to do |
|---|---|---|
| `the cluster is not quorate; nothing is written` | `/etc/pve/.members` says that the cluster has no quorum. The cluster filesystem takes no write then, and the lease is not refreshed: the cycle holds before it saves anything, also when it has nothing to save. Connectors, the egress filter and the gateway keep running. | `pvecm status`. The daemon goes on by itself once the cluster is quorate. |
| `the quorum of the cluster is not known: <why>; nothing is written` | The members file was not read yet, or its last read is more than 5 seconds old: the cluster filesystem does not answer. The cycle holds. | `systemctl status pve-cluster`. |
| `this node does not hold the lease; node <name> is the controller` | Another node holds the lease and writes. The cycle holds. | Nothing: this node serves its connectors, and the controller writes. |
| `this node does not hold the lease yet: <why>; nothing is written` | The lease keeper has not taken the lease: it waits for the lock to expire after another node held it, or, on a node of a cluster, an earlier boot of this one (up to two minutes after a crash), the cluster is not quorate, the node is not eligible, or the lease keeper could not start. The cycle holds. | Usually nothing: the next tries take it. `<why>` names what keeps it. |
| `the lease is fenced: <why>; nothing is written` | This node holds the lease, but may not write now: a refresh failed or is too old, the process or the node stood still (a pause, a suspend), the wall clock stepped, or an agent follows an epoch this controller did not draw, as when another controller writes beside it. Nothing is written until a refresh succeeds again; then the same epoch resumes. | Look at `journalctl -u pco` for the refreshes; when another controller writes, find it before anything else. |
| `the lease lets no write through: <why>`, with the prefix of the run, as in [the writer](#the-writer) | The gate of the lease refused a write the run was about to make: the same causes. The run writes nothing more. | As above. |
| `the store is older than what the agents remember (restored?); pco promote shows the differences and draws a new epoch` | An agent follows an epoch above the one `leader.json` names, before this controller wrote anything: the store went back, as after a restore of `/etc/pve/pco` from an earlier copy, or a second controller drew it. The controller only observes. | [`pco promote`](cli/pco-promote.md) shows what differs and, confirmed, draws a new epoch above every sentinel and every agent; the daemon is then in observe-only mode until `pco apply`. |
| `the agents of <node> (epoch <epoch>), ... follow an epoch above this controller's; pco promote draws above it, or forget a node that should not` | Beside the line above, or beside a fence that says another controller writes: the nodes whose agents reported the epoch that keeps the controller from writing. Only an epoch an agent of this install on a host node reported counts, from a node the registry holds neither removed nor contested, and only one a draw makes. The line stays until `pco promote` draws above every epoch reported. | When the node is yours, `pco promote`. When it should not report that epoch, find out why before anything else: its store or its agent's files may be of another time. |
| `the store went backwards (restored?); pco promote adopts it` | An object this controller wrote in its tenure was read back at a lower revision, or is gone though it did not delete it: the store was put back under the running controller, from a copy that carries the same `leader.json`, or a file of it was removed by hand. Every write of the store is refused from then on, and the controller only observes. Nothing pco does causes it: `pco setup` run again only adds and updates objects, and `pco uninstall`, which removes them, stops the daemon first. | `pco promote`, which takes the store as it is now. |
| `this store was made in cluster <name> (<fingerprint>); pco promote here makes this cluster its writer` | The install records the fingerprint of the CA of another cluster (`/etc/pve/pve-root-ca.pem`): the store was restored into this cluster, or the image of a node was copied. Nothing is written; the node may hold the lease, which `pco promote` needs to read Cloudflare. | When this cluster is meant to be the writer, `pco promote`, which asks you to type the name of the cluster the store was made in, or its fingerprint when it has none. |
| `the identity of this cluster cannot be read (<error>); nothing is written` | `/etc/pve/pve-root-ca.pem` cannot be read. The cycle holds. | `systemctl status pve-cluster`. |
| `the epoch the agent of this node follows cannot be read (<error>); nothing is written` | `/var/lib/pco/agent/epoch.json` cannot be read. It may hold an epoch above the one `leader.json` names, as after a restore of the store, so the controller writes nothing while it cannot tell. | Look at the file and its permissions. A damaged file is removed by hand; `pco promote` then draws an epoch above every one the other agents report. |
| `waiting for the agents of <nodes> to report to this controller; nothing is written at Cloudflare for up to <time>` | A new controller writes nothing, to the store or at Cloudflare, before every node that is neither down nor removed has reported to it once: one of them may remember an epoch the store does not, and a route must not be withdrawn for a node only because its agent has not found the new controller yet. It waits for at most a minute; a node whose agent has not reported by then is `unknown`, and the controller goes on. What that agent reports later still stops the writes when it follows an epoch the store does not explain. | Nothing, unless a node named stays silent: then look at `journalctl -u pco` on it, and at the agent channel, TCP port 8644. |
| `the agents of <nodes> did not report within the takeover hold; nothing is withdrawn at Cloudflare in this cycle` | The first cycle a new controller writes in, after its takeover hold ended without a report of the nodes named. What their agents would have said is not known, so that cycle only adds: every rule a tunnel serves stays, also one the controller would drop or answer 503 for, no DNS record is deleted and no connector removed. The cycles after it write as they see, and a node that is still silent is `unknown`. | Nothing; if a node named stays silent, look at `journalctl -u pco` on it and at the agent channel, TCP port 8644. |
| `<address> is blocked on every node: its MAC <mac> was seen on <port> of <node>` | The watch of the node named saw the MAC the address is bound to on the port of another guest (a conflict), or saw it leave the guest's own port while the guest did not migrate within ten seconds. No node lets a connector reach the address. The block ends once no agent reported it for ten minutes and the guest's node attests its card again. | Find what is on the port named: a clone of the guest, or a guest with a forged MAC. Stop it; the block then ends by itself. |
| `node <node> has no pco agent with a running watch; routes of other nodes' guests are pinned to their nodes` | A node the cluster lists online has no agent that reports a running forwarding-table watch, so no node can prove the port of a guest on another node: the routes of such guests move to tunnels pinned to their nodes at once. They move back to the shared tunnel once every node watched for ten minutes without a break. | Look at `journalctl -u pco` on the node named; the agent there must run. |
| `<n> nodes could run the shared tunnel, more than 25: the first 25 by name do; name the replicas in cluster.replicas` | More than 25 nodes are active and `cluster.replicas` names none: the shared tunnel runs on at most 25. | Name the nodes that run it in `cluster.replicas`. |
| `tunnel <tunnel> in account <id> has <n> ingress rules, more than the 5000 pco keeps a tunnel under; Cloudflare may refuse its configuration` | A tunnel serves more than five thousand hostnames. Cloudflare documents no limit on the rules of a tunnel; pco names it above five thousand, since the 503 rules of an account go into every tunnel of it, and in a cluster every pinned tunnel carries them too. | Spread the hostnames over accounts, or remove routes that are not used. |
| `node <node> stays joining: it has not reported <n> of the shared routes yet` | A node that joins is admitted only once its agent reported every route of the shared tunnel, so that a route it cannot reach is known before its connector serves it. | Nothing; if it stays, look at the agent of the node. |
| `agent of node <node> reports a second connector <id> on tunnel <tunnel>: it is treated as rogue` | The agent of a node may own one connector of a tunnel, and the one before it for ten minutes after a restart. The node reported a further one: that connector is not counted as pco's, and the next listing of the tunnel at Cloudflare shows it as rogue. | Look at the node: a second `cloudflared` with the tunnel's token runs there. Rotate the tunnel's secret if you did not start it. |
| `node <node> stays joining: <node> fails <n> routes that other replicas reach; they move to pinned tunnels before it becomes active` and `... more than cluster.admissionMaxMoves allows (<max>); it stays joining until pco node admit` | The node does not reach shared routes that the other replicas reach. Up to `cluster.admissionMaxMoves` of them move to the tunnels pinned to their guests' nodes first, and the node is admitted once they did; with more, it is not admitted at all. | Find why the node does not reach them (its network), or raise `cluster.admissionMaxMoves`, or admit it with `pco node admit`. |
| `tunnel <tunnel> in account <id>: fetching its token for the other nodes: <error>` | The controller could not read the run token of a tunnel that another node runs from Cloudflare; that node keeps the token it has. | Check the credential of the account. |
| `reading the moves of routes: <error>; no route is moved in this cycle` and `saving the placements of the routes: <error>` | The store could not read or write where the routes of a cluster are placed. Nothing moves until it can; the routes stay where they are. | Check the cluster filesystem (`pvecm status`). |
| `deleting tunnel <tunnel> in account <id>, which serves nothing: <error>` | A tunnel pinned to a node served no route for ten minutes, or its node was removed, and Cloudflare refused to delete it, as while a connector still runs it. The next cycle tries again. | Nothing, unless it stays: stop the connector of that tunnel on its node. |
| `the watch of the agent of node <node> broke twice within ten minutes; routes of other nodes' guests stay pinned until it runs for ten minutes` | The agent of the node named restarted, or its watch did, twice within ten minutes: it is flapping. Routes stay pinned to their nodes until its watch runs ten minutes without a break. | Look at why the agent restarts: `journalctl -u pco` on the node. |
| `the Proxmox API of <nodes> is not fallen back on: the members file lists no address for it` | The Proxmox client of the controller falls back on the API of the other nodes when the one of its own node does not answer, at the addresses `/etc/pve/.members` lists; it lists none for these registered nodes, as for a node corosync has not seen since it started. The list follows the members file within seconds. | `pvecm status`; nothing, once the node is back in the cluster. |
| `.members lists <n> nodes online that have sent no status (<nodes>): check cluster.agentNetwork and the 8644 rule` | The cluster lists these nodes online, the registry names them, and their agents never reported to this controller since its tenure began a minute ago or more: the agent channel does not reach the controller from them, while corosync works. The nodes are `unknown`; nothing is withdrawn for them. | Check that the nodes' agent addresses (`cluster.agentNetwork`) are reachable from each other and that the datacenter firewall admits TCP port 8644 from the ipset `pco-nodes`. |

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
| `guest <guest>: interfaces not refreshed: <error>` | Proxmox answered the question for the interfaces of a guest with an error nobody expects. An agent that does not run in the guest, a guest that does not answer in time and a refusal of the privilege have no line of their own: the first two leave the guest without addresses, quietly, and the third is the line about the guest-agent privilege above. Not a hold. | Read `<error>`: it is what Proxmox said. |
| `Proxmox lists no guest at all, but <n> guests hold a hostname (<guests>); nothing is changed: check the privileges of the Proxmox API token, or run pco apply --confirm-deletes if they were removed on purpose` | The vanish guard: a complete listing is empty while guests hold hostnames. The cycle holds. | `pco setup --repair`, and `pco doctor`; `pco apply --confirm-deletes` if they were removed on purpose. |
| `<n> of <m> guests that hold a hostname are no longer listed by Proxmox (<guests>); nothing is changed until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose` | The vanish guard: more than 5 of the guests that hold a hostname, and more than 30 per cent of them, are gone from the listing at once. This is what a token that lost its privileges looks like. The cycle holds. | As above. |

## Hostnames, claims and routes

| Line | Meaning and cause | What to do |
|---|---|---|
| `settings contain an invalid allow or deny pattern; nothing is changed until it is fixed` | A pattern in `allowHosts` or `denyHosts` does not normalise. The cycle holds. | Fix the pattern; see [Settings](settings.md#hostname-patterns). |
| `hostname <hostname> of <owner> is not published: <reason>` | The hostname policy refuses a route that is the apex of a zone or a wildcard, which a guest publishes only when an `allowHosts` pattern names it. `<reason>` is `the apex of zone <zone> is published only when allowHosts names it: add "<hostname>" to allowHosts`, or `a wildcard is published only when an allowHosts pattern names it: add "<hostname>" to allowHosts`. The route is `rejected`, takes no claim and has no rule or record. | Add the pattern, or take the name out of the Notes. |
| `<n> routes are held back: their identity level is <level>, below the required <level>; guests on other nodes and trusted static addresses are proven at observed only: lower identityMinimum in the settings to serve them` | `identityMinimum` is above what the proof of those routes reaches; they answer 503 and have no record. For one route the line says `1 route is held back: its identity level is`. With `identityMinimum: filtered` in the appliance the line ends with why `filtered` is not reached: `filtered is proven only on the managed network pconet, which is not enabled on this node`, the nftables backend, or `filtered lost: <why>`. | See [a route is held back by the identity minimum](troubleshooting.md#a-route-is-held-back-by-the-identity-minimum). |
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
| `<file>: <address> is not a service address` | A file in `/etc/pve/pco/svcaddrs`, which maps an address behind a leg to the address the connectors dial for it, names one outside the pool of the service prefix, `198.18.1.0` to `198.18.255.255`, as after an edit by hand. The connectors would be sent to that address, so no service address is read or given out until the file is fixed, and nothing behind a leg is reached meanwhile. | Delete the file, or put back the copy from before the edit; the address behind the leg then gets a new service address. |
| `the pool of service addresses says next is <address>, outside 198.18.1.0 to 198.19.0.0` | `/etc/pve/pco/meta/svcpool.json`, the next service address to give out, is outside the pool, as after an edit by hand. No service address is given out until it is fixed: one might be given out twice. | Put back the copy from before the edit. Without the file, pco starts after the highest service address in use, which is safe only as long as no other was given out after it and released. |

## The gateway and service addresses

A guest that pco reaches through a leg of its own, in an attached network or the managed one,
is published under a service address: the connectors dial that address, and the gateway
forwards it to the guest. On the host the gateway is the network namespace `pco`, which
`pco-netns.service` brings up at boot; in the appliance the legs are network cards of the
container itself, `net1` to `net9`, which the daemon adds to its own configuration through the
Proxmox API, and there is no namespace and no `pco-netns`. Where the lines below name `pco netns
status`, look at the journal (`journalctl -u pco`) in the appliance. [Networking](networking.md)
is the page of the legs and the service addresses.

| Line | Meaning and cause | What to do |
|---|---|---|
| `hostname <hostname> is not served through network <network>: service address not allocated: <reason>` | The guest was proven through the leg, but has no service address yet, and none is made while the reason stands; the hostname answers 503 meanwhile. A new service address is made only when this process is known to be the writer (the reasons are below). Its real address is never sent to a connector. | Usually nothing: the next cycle makes it. A reason that stays says what to fix. |
| `hostname <hostname> moved to <address> on network <network>; its rule is kept, and its service address <service> forwards nothing until it can follow: <reason>` | The guest of the route was proven at another address, or behind another network, while no service address can be made. The tunnel keeps its rule, so nothing is written at Cloudflare, but the service address forwards nothing from the cycle that saw the move: whoever is at the address it left is never reached through it. Once a service address can be made again, it follows the guest to its new address, still without a write at Cloudflare, and the route answers again. | As above. |
| `programming the gateway: <error>; no tunnel configuration is written until it is programmed` | The legs, the service addresses or the neighbours could not be put in the gateway. No tunnel configuration is written: a rule must not send a connector to a service address the gateway does not forward. | `pco netns status`; the journal says why. On the host, `systemctl restart pco-netns` brings the namespace back. |
| `network <network>: the leg is not up: <why>` | The leg of the network could not be raised, as when its bridge is absent or another machine holds its address; in the appliance also when its card cannot be added, as when every slot `net1` to `net9` is taken. Guests behind it are not reached through it. | Fix what it says; `pco netns status`. |
| `network <network>: the leg is up, but <what>` | The leg is up, with something the gateway left out, as a service address it refused. | `pco netns status`. |
| `saving the cache of the gateway: <error>; a boot before it is saved brings the gateway back as it was before` | What the gateway serves could not be written below `/var/lib/pco/netns`, which `pco-netns.service` restores the host's namespace from at boot before the guests start, and which the daemon starts from in both profiles. | Free space or permissions of `/var/lib/pco`. |
| `releasing the service address <service> of <address> on network <network>: <error>; the next cycle releases it` | A service address no route uses any more could not be deleted from `/etc/pve/pco/svcaddrs`. | `pco doctor` has the check `store`. |
| `<n> frames from MACs no lease knows were seen on the managed network past the 64 that have an event; each had the guests read again` | A guest sends from many MACs on the managed network, more than have an event of their own. Each frame still has the guests of the node read again and, in the appliance, the isolation of the managed network dropped until a cycle proves it again. | On the node, `bridge fdb show br pconet` lists the port each MAC came from; the port names the guest (`tap<vmid>i<n>`, `veth<vmid>i<n>`, `fwpr<vmid>p<n>`). |
| `the gateway namespace was changed outside pco and was brought back` | Something changed the namespace `pco`, or in the appliance the legs, routes or table of the container; the daemon put them back. The line carries what differed. | Find what changes it. |
| `the gateway namespace was changed outside pco and could not be brought back: <error>` | The same, and it could not be put back. On the host a namespace that is gone stays gone until `pco-netns` runs again, which the daemon cannot do from its sandbox. An appliance whose self-identification has not passed leaves this to its next cycle. | On the host `systemctl restart pco-netns`; it brings the namespace back and stops nothing. In the appliance the journal says why. |
| `checking the gateway namespace: <error>` | The namespace, or the legs of the appliance, could not be read. | `pco netns status`. |

The reasons a new service address waits for: `no cycle has run since the start` (after every
start of the daemon, for one cycle: service addresses that exist are used at once), `the
previous cycle did not get to the tunnel run`, `the previous cycle found a <verdict> writer`
and `the previous cycle could not tell the writer` (see [the writer](#the-writer)), `the writer
identity could not be read this cycle`, `leader.json changed since the cycle began`,
`leader.json could not be read again: <error>`; in the appliance also `the self-identification
of the appliance did not pass this cycle`, `the cluster is not quorate`, `the quorum of the
cluster could not be read: <error>`, `this process drew a new epoch and none of its writes is
verified at Cloudflare yet` and `the previous cycle found this appliance behind its last write
at Cloudflare`. Where no gateway runs, the reason is `no gateway runs on this node`; for a
network with no leg here, `network <network> has no leg on this node`; for the address a route
just left while its service address waits to follow it, `service address <service> of this
address waits for the route that left it`; for an address whose service address was made for
another owner and could not be released (the line of the release says why), `service address
<service> of this address was made for another owner and could not be released`; for an
address where the guest its service address was made for is proven too, `service address
<service> of this address was made for <owner>, which is proven there too` (one of the two
proofs is wrong; neither guest is given the other's service address); for a guest missing
from the inventory of this cycle, `the identity of guest <guest> is not known in this cycle`
(a service address it already has is kept). A pool that ran out gives the line of its error:
`every service address was given out: ...`. A route whose service address waits to follow it
is `unreachable` with the reason `service address not moved: <reason>; the rule is kept and
reaches nothing until it is`.

## Networks behind a leg

| Line | Meaning and cause | What to do |
|---|---|---|
| `network <id>: pco reaches at most 15 networks: the SNAT through each takes a block of 4096 source ports, and 15 blocks end at port 62463, where one more would pass 65535`, and the same words after `network <id>: mark <n> is not 1 to 15:` | A 16th network was refused. The SNAT through the leg of each network draws its source ports from a block of 4096 of its own, and 15 blocks are all that fit below port 65536. The marks are kept once for the whole cluster, so the 15 are those of the cluster, and the limit is in the code, not a setting. With the blocks a target takes about 68 new connections a second; see [Architecture](architecture.md#the-network-gateway). | Remove a network that nothing you publish needs before adding another. |

## The managed network

The managed network is the SDN vnet `pconet`, on which pco gives consenting guests a card with an
address of `managed.subnet`. `pco managed status` shows the cards, the guests that wait and why,
and the foreign cards; [Networking](networking.md#managed) is its page, and
[Troubleshooting](troubleshooting.md#networks-behind-a-leg-and-the-managed-network) goes through
the cases.

| Line | Meaning and cause | What to do |
|---|---|---|
| `the vnet pconet was changed outside pco: <why>` | The vnet is not as pco made it: `it does not exist`, `its zone is of type <type>, not simple`, `isolate-ports is off, and on a host it is on` (or on, and off in the appliance), `it has <n> subnets, and pco's leg is its only gateway`, or `it could not be read: <error>`. No card is added while this stands; the cards that exist stay. | Put the vnet back as `pco network managed enable` makes it, or remove the subnet. |
| `the managed network cannot run on this node: <why>` | In the appliance, the node cannot isolate the guests: `the firewall of node <node> runs on the nftables backend (nftables: 1), which installs no guest rules; set nftables: 0 in its firewall options`, `the datacenter firewall is off; no guest rule is in force until it is turned on`, `pve-firewall is <state> on node <node>; no guest rule is in force until it runs`, or the options could not be read. No card is added; the cards that exist stay. On a host whose daemon cannot change guests: `this daemon cannot change the cards of guests`. | What the line says, on the node. |
| `managed.consent is auto but the gate tag <tag> is not registered as admin-only; automatic attaches are stopped; attached guests stay attached` | Automatic consent rests on a tag only an admin may set; the gate tag left `registered-tags`. Nothing is attached automatically and nothing is detached. | `pco setup --repair` registers the tag again. |
| `managed.consent is auto, and registered-tags could not be read; automatic attaches are stopped; attached guests stay attached` | The options of the datacenter could not be read, which takes `Sys.Audit`. | `pco setup --repair`; `pco doctor`. |
| `foreign card net<n> (<mac>) of <guest>: <why>` | A card on `pconet` that no attachment explains, as a clone or a restore copies one. A tagged guest that would be attached gets its own card in its place; any other has its link set down where pco may write the guest (a container that refuses `link_down` loses the card), and is only reported where it may not. | Remove the card where pco only reported it. |
| `the DHCP server is not listening on the managed leg: <error>` | The server that gives the virtual machines on `pconet` their addresses could not open its sockets on the leg, or stopped: the leg is gone or down, or on a host the namespace `pco` is not there. It is started again every few seconds, and again in a new namespace `pco` after `systemctl restart pco-netns`; once it listens the line goes. Containers are not affected: their address is in their configuration. At its start the host's daemon waits up to 10 seconds for the server, or until its first start failed, before it tells systemd that it is ready. | `journalctl -u pco`; on a host `pco netns status`, and `systemctl restart pco-netns` brings the leg back. |
| `managed card of <guest>: <error>` | A card could not be added or removed, or the attachment could not be saved: `qm` or `pct` refused, as for a card on another bridge at the index pco's card had, or the API did. In the appliance also when the guest firewall that isolates the card could not be read or written (`the firewall of the card could not be written: <error>`; an ipset `ipfilter-net<n>` that pco did not make is left alone and refuses the card), kept as written (`its firewall: <error>`), or put back after a detach (`putting its firewall back: <error>`); pco's rules that still read back otherwise after they were written (`its firewall: pco's rules on the card still differ from what pco writes after they were written; they are written again once they change`); the link of a card that comes up only at the restart of a virtual machine without hotplug. No card is added without its firewall. The attachment is kept and the step is tried again in the next cycle. | Read the error. The token needs `VM.Config.Network` on the guest for its firewall too. |
| `filtered lost: <why>; the routes on pconet are served at observed` | In the appliance, a guest on `pconet` is not proven isolated by its firewall: the backend, the datacenter firewall or `pve-firewall` (as in the line above), a foreign card, `guest <owner>: net<n> has firewall=0`, `net<n> is not yet enforced: pco wrote its firewall <time> ago` (less than 15 s ago: a new card stays link down until then, and a card whose firewall pco just put right is not counted yet), `the guest firewall is off`, `macfilter is off`, the ipset `ipfilter-net<n>` missing or not holding exactly the lease, pco's rules on the card changed, missing or after a rule that is not pco's, `the firewall is not enforcing on net<n>` (the canary from the leg, a SYN to a port no route publishes, was answered with a SYN-ACK or a reset), the vnet changed, or a frame from a MAC no lease knows. The routes on `pconet` drop to `observed` until a cycle proves it again; with `identityMinimum: filtered` they are held back. pco puts its own ipset and rules right in the next cycle; the guest firewall's options it leaves as the admin set them. | What the reason names. A guest that drops the canary with a firewall of its own passes. |
| `no free address on the managed network: <subnet> holds <n> leases, ...` | The subnet ran out of addresses; the guest waits. | Detach guests that no longer need the network, or a larger `managed.subnet` before the network exists. |
| `the managed network is left as it is: <doing>: <error>` | The networks, the consents, the attachments or the leases could not be read. Nothing is attached or detached in this cycle. | The error names the file; see [Files](files.md). |

## The appliance

These lines come from a daemon in the appliance, where the container proves in each cycle
that it is the one that was installed, and that no principal of Proxmox can reach into it.
A container that is a copy, and one that a principal can reach into, stop serving: the
identity flag goes, the egress filter is emptied and the connectors are stopped.

| Line | Meaning and cause | What to do |
|---|---|---|
| `/var/lib/pco has no pco volume marker (restore, or a volume that is not pco's?): run pco appliance repair --vmid <vmid> on the node`, `/var/lib/pco is not a mount point of its own (a restore without the volume?): run pco appliance repair --vmid <vmid> on the node` | The daemon found no state volume of pco, as after a restore, and exited with status 78 before it opened anything; the unit stays failed. `pco status` says this line, as there is no daemon to ask. | `pco appliance repair --vmid <vmid> --recover` on the node; see [Appliance](appliance.md#restore-and-repair). |
| `pco has no state on its volume (restore?): run pco appliance repair --vmid <vmid> on the node` | The volume has the marker and no store: a repair or an install did not finish. The daemon writes nothing and refuses every action. | The repair the line names. |
| `the install of this appliance records no appliance; nothing is changed` | The install record has no appliance block. The cycle holds. | `pco appliance recover`. |
| `the volume at /var/lib/pco is <volume>, a volume of VMID <n> and not of <guest>: this container is a copy; the connectors are stopped and the egress filter is empty` | The state volume belongs to another container: this one is a copy. | Do not run the copy. Remove it, or recover it on purpose. |
| `Proxmox lists no <guest>: this container is not the one installed; the connectors are stopped and the egress filter is empty` | No container of the VMID it was installed as exists. | The same. |
| `<reason>; the connectors are stopped and the egress filter is empty` | Nothing proves that this container is the installed one: its state volume has no form pco knows and another guest carries its MAC, Proxmox reports no uptime of it, or its uptime differs from the one Proxmox reports. The reason names the facts. | `pco appliance repair` on the node, if it is the appliance. |
| `<principal> holds <privileges> on the appliance lxc/<vmid>; pco serves nothing while it does: <commands>` | A principal other than an admin can reach into the appliance. The connectors stop while it can. | The line gives the `pveum acl modify ... --roles NoAccess` commands. |
| `the access control of Proxmox has not been read since pco started; the connectors start once it shows that no principal can reach into the appliance` | After a start, the connectors wait for the first read of the access control. | Nothing. |
| `the inventory is incomplete; self-identification waits for a complete one`, `the uptimes of the guests could not be read; self-identification waits for them`, `the facts of this container could not be read: <error>` | The check cannot be made yet. Writes are held until it can. | Passes; if not, look at the error. |
| `the links of this container carry <MACs>, but <guest> in Proxmox has <MACs> for more than 60 s; writes are held until they match` | A network card of the container and its configuration in Proxmox disagree. | `pco appliance repair`, or put the configuration right in Proxmox. |
| `<guests> in pool pco carries a MAC of <guest>: a copy of the appliance runs; writes are held until it is gone` | A guest of the pool of the appliance carries its MAC. | Remove the copy. |
| `<guest> runs with the MAC <mac> of the appliance <guest> and takes the appliance's traffic on its network: remove it if it is a copy of the appliance, as a restore or a clone without a new MAC is, or give its card another MAC`, and for a guest that does not run `<guest> is configured with the MAC <mac> of the appliance <guest> and takes the appliance's traffic on its network once it is started: remove it if it is a copy of the appliance, as a restore or a clone without a new MAC is, or give its card another MAC` | A guest other than the appliance is configured with one of its MACs, outside the pool `pco` while it runs: a restore of the appliance to another VMID without `--unique`, which lands outside the pool, or a guest given the MAC on purpose. A restore of the appliance stops itself as a copy, but its card answers for the MAC all the same. The cycle finds it in the configurations it reads anyway, at least every five minutes; not a hold. | Remove the copy, or give its card another MAC. `pco doctor` names the guest at once in its `shared mac` check. |
| `the certificate of <address> no longer verifies under <name> (the cluster CA or the pveproxy certificate changed?): run pco appliance repair --vmid <n> on the node` | The appliance cannot verify the Proxmox API any more. | The command the line gives. |
| `this container is <guest>, but its writer epoch could not be decided: <error>` | The identity is proven and the epoch could not be saved. | Look at the error. |
| `a NIC of the appliance <guest> appeared or vanished within the last minute; its links and its configuration in Proxmox must match by then` | A network card of the container and its configuration do not agree yet. Not a hold. | Passes within a minute. |
| `writing the identity flag: <error>; the connectors do not start until it is written`, `removing the identity flag: <error>`, `emptying the egress filter: <error>`, `stopping the connectors: <error>` | A step of starting or stopping the connectors failed. | Look at the error. |
| `the cluster is not quorate; nothing is written` | Without quorum the cluster filesystem is read-only, so nothing could be saved. The cycle holds. | Restore the quorum of the cluster. |
| `the quorum of the cluster could not be read (<error>); nothing is written` | The same, when the status could not be read. | Look at the error. |
| `the state of this appliance is older than its last write at Cloudflare (rollback or restore); run pco appliance recover` | The volume was rolled back or restored, so the writer behind it is older than the one at Cloudflare. The cycle holds. | `pco appliance recover`. |
| `leader.json belongs to an earlier start of this container; a new epoch is drawn once self-identification passes` | The container started again; the writer gets a new epoch after the identity is proven. The cycle holds meanwhile. | Nothing. |

A guest outside the pool `pco` that runs configured with a MAC of the appliance also has an issue
of its own, `configured with the MAC <mac> of the appliance lxc/<vmid>`, and its routes are
rejected; see [Troubleshooting](troubleshooting.md#a-guest-with-the-appliances-mac).

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
| `web` | `info`, `warn` | What the daemon did for the web interface: `web certificate renewed: <why>, fingerprint <fingerprint>`, when it made a new key and certificate of the cluster CA because the old one ends within 30 days, another CA signed it, it lacks a name or address the node has now, or `tls.crt` is a link; in the appliance `web certificate made for <names>, fingerprint <fingerprint>` at its first start and the same renewal 30 days before its self-signed certificate ends. The subject is `web certificate`. In the appliance, with the subject `web listen` and the level `warn`: `net0's address changed from <old> to <new>: pco-web listens on <new>:8643 now`. | Nothing; after a new address of net0, open the web interface at the new address. Its certificate, and the fingerprint, stay. |
| `net` | `warn`, `error` | What `pco-net.service` keeps in the appliance was changed and loaded again, or could not be. The subject is `service prefix`. | `pco net show`. |
| `managed` | `info`, `warn`, `error` | A guest got a card on the managed network (`<guest> is attached to the managed network: net<n> at <address>, by <consent>`), or its card waits for its restart; its card goes after the grace (`the card net<n> of <guest> on the managed network goes at <time>: <why>`), stays after all, was removed, or the guest is detached and its address free again; the guest took its card to another node; a foreign card was replaced by the guest's own, set link down, removed or reported; automatic attaches stopped or resumed; the vnet was changed outside pco or is as made again; the managed network cannot run on the node or can again; the subnet ran out of addresses; the source-MAC watch saw a frame of a MAC no lease knows; in the appliance, `filtered` was lost (`filtered lost: <why>; ...`) or holds again, the link of a new card came up once its firewall was enforced, and the firewall of a detached guest was put back as it was, or stays on because an admin changed its policies or added rules since pco turned it on; an admin switched the firewall-impact confirmation, or enabled the managed network. The subject is the guest, or `pconet`. | See [the managed network](#the-managed-network). |
| `network` | `info`, `warn`, `error` | An admin attached a network (`network <network> is attached: the leg <leg> with <address> on <bridge>, mark <mark>; ...`) or detached one, with who asked (`pco network attach`, `pco network detach`). A leg came up (`the leg <leg> of network <network> is up`), is not up (`... is not up: <why>`) or is up with a problem. A service address was made for a guest behind a leg (`service address <service> allocated for <address> on network <network>`), followed a route to its new address (`service address <service> moved from <address> on network <network> to <address> on network <network>, with <hostname>`), or was released once no tunnel sends a connector to it and no route is bound to the guest any more (a withdrawn route keeps it, so that a guest whose MAC changed is published where it was once its proof is back; another guest proven at that address gets a service address of its own, and the kept one is released, so that the withdrawn route's rule reaches nothing), or at once when no route can take it along. The subject is the network. Or the gateway namespace was changed outside pco and brought back, or could not be; the subject is `pco`. | See [the gateway and service addresses](#the-gateway-and-service-addresses). |
| `node` | `info`, `warn` when a node is `unknown` or `down` | A node of the registry changed state (`node <name> is <state>, was <state>`: a node that joined and reports is admitted in the next cycle, one whose agent sent no status for 30 seconds is `unknown`, one the cluster lists offline is `down`, one whose agent started again passes through `joining`), got the index its leg MACs are made from (`node <name> has index <n>`), or was drained or undrained because Proxmox put it into maintenance mode or took it out (`cluster.followMaintenance`); or the controller refused a status of its agent as too large, or a resend of its events past 10 000. The subject is the node. | `pco status --json` lists the nodes under `nodes`. |
| `attestation` | `warn` | The controller did not take what an agent said of the ports of guests: the agent is not a host agent (an appliance reads no bridge), or it said more than the controller reads of one status (1000 attestations, 500 observations). Said once per node and reason; part of the audit trail. The subject is the node. | Look at the node named: an agent that says more than it can see is broken or not pco's. |
| `block` | `error` when an address is blocked, `info` when the block ends | An agent's watch saw the MAC of a published guest on the port of another guest, or leave the guest's port while the guest did not migrate: the guest's address is blocked on every node (`<address> is blocked on every node: its MAC <mac> was seen on <port> of <node>`), until no agent reported it for ten minutes and the guest's node attests the card again. The subject is the address. | Find the guest on the port named; a clone with the guest's MAC, or a forged MAC, is what blocks it. |
| `placement` | `info` | In a cluster, a route moved between the shared tunnel and a tunnel pinned to a node: a move begins (`<owner> moves from <tunnel> to <tunnel>: <why>`), is given up because a new controller took over before the records were switched or the route belongs where it was again, ends once the old rules are gone, or a hostname is placed elsewhere (`<hostname> is placed pinned to <node>, was shared`). A tunnel pinned to a node is deleted once it served no route for ten minutes, or its node was removed. The subject is the owner, the hostname or the tunnel. | `pco status --json` shows the placement of each route under `routes[].placement`. |
| `enrol` | `info` | The agent endpoint took the certificate of a node: the registry follows the certificate a host node issued itself and wrote into its enrolment, or, in the observe-only window after a promotion, records one the store held none for. The event is of the audit trail, also kept in `/etc/pve/pco/security-events`. The subject is the node. | `pco events`. |
| `lease` | `info`, `warn` when it is lost or fenced, `error` when the controller is behind | What happened to the lease of the host: `acquired at generation <n>`, `fenced at generation <n>: <why>`, `resumed at generation <n>`, `this node no longer holds the lease: <why>`, `behind at generation <n>: <why>; nothing is written until pco promote`, `promoted to generation <n>; observe-only until pco apply`, and `released as the daemon stopped`, or on a node that stands alone `kept as the daemon stopped: this node stands alone, and its next start keeps the epoch`. The subject is the node. | See [the lease](#the-lease). |

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
| `outbound` | TCP to `region1.v2.argotunnel.com:7844` cannot be made. A warning when every connector is connected all the same, over QUIC perhaps, and the last cycle did not hold (a cycle that held has not looked at the connectors); else a failure. | Allow outbound TCP and UDP to port 7844. |
| `proxmox` | The Proxmox API does not answer with the token of pco (fails), the version cannot be told (warns), or it is older than 8.4 (fails). | Check the token with `pco setup --repair`; upgrade Proxmox VE. |
| `store` | The store is not mounted (fails) or not set up (fails). In the appliance it is the volume with its marker and the store on it, also when the daemon does not run, as root. | `systemctl status pve-cluster`; `pco setup`. In the appliance: `pco appliance repair --vmid <vmid>` on the node. |
| `node lock` | This daemon does not hold the lock of the node (fails). | `systemctl restart pco`, so that no second daemon can start. |
| `egress` | The filter is switched off, its table is not loaded or not the one pco loads (fails: the connectors are not confined); the daemon has not checked yet, or found it in a state it does not know (warns). Without a daemon it also fails when the connector user does not exist. | `pco egress on`, or `pco egress show` and `pco egress load`. |
| `nftables` | `nftables.service` is enabled (warns): when it starts or restarts, its ruleset flushes the egress table. In the appliance the unit is expected to be masked: enabled fails, because its ruleset flushes both tables of pco; any state but masked or not installed warns; so does an answer that systemd does not give. | `systemctl disable nftables.service`, or keep its rules from flushing the whole ruleset. In the appliance: `systemctl mask nftables.service`. |
| `mode` | Observe-only mode (warns). | `pco apply`. |
| `inventory` | The inventory is incomplete (fails). | `pco status` lists the problems. |
| `writer` | The verdict is `stale`, `foreign` or `unknown` (fails); in the appliance also `behind`, the state older than the last write at Cloudflare. | See [Writer verdicts](#writer-verdicts). The command differs by profile: `pco setup --recover` on a host, `pco appliance recover` in the appliance, and `pct exec <vmid> -- pco appliance recover` from the node for `behind`. For `unknown` in the appliance, `pco status` lists the problem that says why and what to do, and without a credential the fix is `pco credential add --label <label>`. |
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
| `connector <name> in account <id>` | The unit does not run, the last cycle found no connector for the tunnel, or the connector is not connected to Cloudflare (fails); systemd did not say whether the unit runs, or it runs and whether it is connected was not checked in the last cycle (warns). | `systemctl status` and `journalctl -u` of the unit. |
| `web certificate` | Only where setup made the web interface: the certificate cannot be read, does not match its key or has expired (fails); it ends in less than 30 days (warns, in the modes `ca` and `own`). | `pco web cert renew`, or `pco web cert import <crt> <key>` for a certificate of your own, or `pco setup --repair` for the mode `pveproxy`. |
| `unit <unit>`, `daemon` | Only while the daemon does not answer, as root: the unit `pco.service` or `pco-egress.service` does not run (fails) or does not start at boot (warns); `daemon` warns that the checks that need the daemon were not made. | `systemctl enable --now <unit>`, and `journalctl -u <unit>`. |
| `doctor` | The run of the checks stopped on a defect (fails). | `journalctl -u pco` shows where it stopped. |

### Checks of the appliance

These run only in an appliance, where the daemon knows its container, its volume, its network
and its place in Proxmox. A check that cannot read what it looks at warns, `<what> could not
be read: <error>`, with the fix `the api and proxmox checks say why Proxmox does not answer`;
the rows below do not repeat that. A read of the container's own disk or `/proc` that does not
answer in 5 seconds, as one of a storage that stalls does not, is an error `no answer within 5s`
in the same way, and a check that has not ended after 30 seconds fails with `no answer within
30s: something it reads may be stalled`, with the fix to look for a mount or a storage of the
container that does not answer (`pct config <vmid>` on the node lists them); the other checks
are answered all the same. A check that stops on an internal error, as by a panic, is a failure
named like the check, `the check stopped on an internal error: <error>`, with the fix
`journalctl -u pco may say more; report it`; it takes no other check down. `<vmid>` is the
number of the container, and a fix that says `on the node` is run on the Proxmox node and not
in the appliance.

| Check | Fails or warns when | Fix |
|---|---|---|
| `access` | The access control of Proxmox cannot be read (fails: no connector starts until it is, and nothing says that no principal can reach into the appliance), or a principal other than an admin and pco's own holds privileges on the appliance (fails: the connectors are stopped meanwhile). | `pveum acl modify <path> --users <user> --roles NoAccess`, or `--tokens` for a token that separates its privileges, as the line gives it; for an unreadable access control, check that the token holds role PCO on `/` (`pveum acl list`). |
| `api` | The certificate of the Proxmox API no longer verifies under the server name it was installed with, as when the cluster CA or the `pveproxy` certificate changed (fails), or the API does not answer (fails). | `pco appliance repair --vmid <vmid>` on the node; or check that `pveproxy` runs and that the appliance reaches port 8006. |
| `clock` | The clock is 30 seconds or more away from the `Date` of Cloudflare's answer (warns), or 5 minutes or more (fails); or Cloudflare does not answer (warns). | The container shares the clock of the node: check its time synchronisation, chrony or `systemd-timesyncd`. |
| `cloudflare api` | `api.cloudflare.com` does not answer for pco (warns). Every change at Cloudflare needs it. | Let the appliance reach `api.cloudflare.com` on port 443. |
| `cmode` | The console mode is not `tty`, now or at the next start (fails): the console is then a root shell without a password, and any one `VM.Config` privilege sets it. | On the node: `pct set <vmid> --cmode tty`, or `pct set <vmid> --revert cmode` for a pending value, and restart the container. |
| `disk` | `/` or `/var/lib/pco` is 80 per cent full (warns) or 90 per cent (fails), or its use cannot be read (warns). | `apt-get clean`; on the node `pct resize <vmid> rootfs +1G` or `pct resize <vmid> mp0 +1G`. |
| `dns` | No name server is configured or the file cannot be read, or the name servers do not answer for `region1.v2.argotunnel.com` (fails): the connectors cannot resolve the edge. | On the node: `pct set <vmid> --nameserver <address>`, then restart the container; `pct config <vmid>` shows what it was given. |
| `egress probe` | As the user `pco-connector`, which the egress filter confines, `region1.v2.argotunnel.com:7844` does not connect, or the address of the Proxmox API connects or fails in another way than refused (fails); the user `pco-connector` does not exist (fails); the address of the API is refused but the API does not answer for pco either (warns: the node refuses a port nothing listens on in the same way, so the refusal then says nothing of the filter); the install records no address of the API to probe (warns). | For the edge, a way out to Cloudflare on port 7844, TCP and UDP; for the API, `pco egress show` shows the table and `pco egress load` loads it again; for the user, install the package again or run `systemd-sysusers`; for an API that does not answer, see the `api` check. |
| `epoch` | Never: it has a finding only when this daemon drew a new epoch after a start of the container, and says so. After a rollback, approvals, acknowledged segments and credentials made after the snapshot are gone. | Nothing. |
| `etc-pve` | `/etc/pve` exists in the container (fails): a bind mount of the cluster filesystem, which with the node's `www-data` group mapped in lets the container read the TLS keys of the node. It also fails when `/etc/pve` does not answer: something is mounted there that stalls. | On the node: `pct set <vmid> --delete <mpN>` for the mount point whose `mp` is `/etc/pve`; the line names it when the configuration shows it. |
| `features` | The features of the container are not exactly `nesting=1`, now or in the pending configuration (fails). | On the node: `pct set <vmid> --features nesting=1`, or `pct set <vmid> --revert features`, and restart the container. |
| `firewall` | Never; it says whether the datacenter firewall and the firewall of the node are on or off. It warns only when they cannot be read. | Nothing. |
| `holds` | `pco` or `cloudflared` is not held by apt (warns): apt or `unattended-upgrades` may replace it. | `apt-mark hold pco cloudflared`. |
| `identity` | No cycle has run, or no self-identification has (warns); the container is a copy, or the self-identification did not pass (fails). | For a copy, stop it; a restored container becomes the appliance with `pco appliance repair --vmid <vmid>` on the node, once the original is gone. Otherwise see [the appliance](#the-appliance). |
| `journal` | The journal takes 64 MiB or more (warns), where the appliance keeps it to 64 MiB. | `journalctl --vacuum-size=32M`, and look in `journalctl -p warning` for what writes so much. |
| `memory` | Tasks waited for memory in more than 10 per cent of the last 10 seconds (warns). The pressure is that of the container's own cgroup, and the line gives what the container uses of its limit; where the cgroup does not report it, the line says that it is the pressure of the node, which the container shares, and more memory for the container would not change that. It is fine, and says so, where the kernel reports no pressure at all. | For the container: on the node `pct set <vmid> --memory <megabytes>`; the line suggests twice the current value. For the node: `cat /proc/pressure/memory` and `free -m` on the node. |
| `net` | What `pco-net.service` loads is not in place (fails); packets were sent to the service prefix without a mapping and rejected since the table was loaded, or they cannot be counted (warns). | `pco net show`. The daemon loads the table again within 30 seconds. A few packets after a route was withdrawn are usual; a count that grows is not. |
| `network grants` | pco's user or its token may put cards on a whole zone, every bridge or vnet of it, present and future, or on a bridge that carries an address of the node (warns). | On the node: `pco appliance revoke-network --vmid <vmid> --bridge <bridge>` (with `--vlan`), `pveum acl delete` for a whole zone, then `pco appliance grant-network` one network at a time. |
| `protection` | The container is not protected against removal (fails): one command can destroy it with the volume of its state. | On the node: `pct set <vmid> --protection 1`. |
| `quic buffer` | `net.core.rmem_max` is below 7500000, the receive buffer `cloudflared` asks for its QUIC connections (warns). It is a setting of the node, which a container cannot change. | If you want it, on the node: `echo net.core.rmem_max=7500000 >> /etc/sysctl.d/90-pco.conf`, then `sysctl --system`. |
| `segment access` | A principal other than an admin and pco's own may use `SDN.Use` on the segment of the appliance's first card (warns): a guest put on it with the MAC of the appliance cuts off its inbound traffic. The path is `/sdn/zones/<zone>/<bridge>` for a Linux bridge, in the zone `localnetwork`, and for a vnet the zone that `cluster/sdn/vnets` gives it, with `/<vlan>` when the card has a tag; a grant on the zone alone counts. It also warns when the card, the zone or the access control is not known. | Put the appliance on a segment only admins may use, or take `SDN.Use` from the principals the line names. |
| `shared mac` | A guest of the node other than the appliance is configured with one of its MACs, the ones the install recorded and those of its configuration: it runs (fails), or it does not (warns). The check reads the configuration of every guest of the node afresh, the first 1000 of them; it warns when a configuration could not be read, and when the node has more guests than that. | On the node: remove the guest if it is a copy of the appliance (`pct destroy <vmid>`, `qm destroy` for a VM), or give its card another MAC: set the card again without its MAC (`pct set`, or `qm set` for a VM), and Proxmox draws a new one. For a node with more guests, `grep -il -e <mac> /etc/pve/nodes/*/lxc/*.conf /etc/pve/nodes/*/qemu-server/*.conf`, as the line gives it, lists the guests with a MAC of the appliance. |
| `snapshots` | The container has a snapshot, bar one named `pco-pre-upgrade-<date>` that is less than 7 days old, or a replication job copies it (warns): each carries a copy of the state volume, with the Cloudflare credentials and the keys. | On the node: `pct delsnapshot <vmid> <name>`, `pvesr delete <job>`. |
| `token` | The token of pco lacks privileges of role PCO on this release of Proxmox VE, or its permissions cannot be read (fails); it holds more than the role gives (warns). | On the node: `pveum role modify PCO --privs <privileges>`, and grant it on `/` to the user and the token, as the line gives it. For more than the role gives, the `pveum acl delete / --roles <role> ...` lines the check names for the lines of the access control on `/`, or `pveum acl list` to find them. |
| `unattended-upgrades` | It is not enabled and configured (warns): the Debian security updates are not installed on their own. | `systemctl enable unattended-upgrades.service`, and keep `APT::Periodic::Unattended-Upgrade "1"` in `/etc/apt/apt.conf.d`. |
| `versions` | It says what runs: pco, `cloudflared` and Debian. `cloudflared` is more than ten months old, or the list of vetted versions cannot be read or is more than 90 days old (warns); the list denies the installed `cloudflared` (fails). | `pco upgrade cloudflared`; `pco upgrade --check` says whether a newer release is out. |
| `volume` | `/var/lib/pco` is not a mount of its own with the volume marker or does not answer (fails), or `mp0` is missing, is mounted elsewhere, or is not kept out of backups (fails): a backup would carry the credentials and the keys. | `pco appliance repair --vmid <vmid>` on the node; for the backup, `pct set <vmid> --mp0 <the value with backup=0>` on the node. |
| `web listen` | Only while `pco-web.service` is enabled. `/etc/pco/net0` names an address that net0's card (`eth0`) does not have now, or the card has no IPv4 address (fails): `pco-web` cannot listen. The daemon writes the card's new address into the file and restarts `pco-web` as soon as the address changes, with a `web listen` event, so this lasts only until it does. `PCO_WEB_LISTEN` names an address other than net0's (warns): the other cards are legs into the networks of guests, which could then reach the sign-in page and try the passwords of the cluster there. It also warns when net0's address, the listen address or the addresses of the card cannot be read, as `pco-web` then refuses to start. | When the daemon did not follow the card, `journalctl -u pco` says why. Take `PCO_WEB_LISTEN` out of `/etc/default/pco-web`, or give it net0's address, then `systemctl restart pco-web`. When net0's address is not known, `pco appliance repair --vmid <vmid>` on the node writes it. A card without an address: check the DHCP server of its bridge, or give net0 a static address. |

### Checks of the networks

These run only where pco reaches guests through a leg of its own: an attached network or the
managed network. A node that reaches every guest directly has none of them, but for `netns`
while `pco-netns.service` is enabled. `netns` and `pconet` are checks of the host, where the
legs live in the namespace `pco`; `filtered pconet`, `managed firewall` and `pool grant` are
checks of the appliance. The checks of the managed network run once it is enabled.

| Check | Fails or warns when | Fix |
|---|---|---|
| `netns` | The namespace `pco` is not pinned at `/run/netns/pco` while a network is attached, the daemon found it changed and could not bring it back or could not check it, or the table `inet pco_host` is not loaded (fails); the loaded `pco-netns.service` has an `ExecStop` line, which an old unit file has and which tears the gateway down on a restart (fails). When the namespace is gone under such a unit, the line names both, and the fix starts with the new unit. It warns while the unit is not enabled with a network attached, when the loaded `pco.service` is not ordered `After=pco-netns.service` (a unit file from before an upgrade, after which the daemon may start before the gateway at boot), and when systemd or `nft` does not answer. | `systemctl restart pco-netns`, which brings the namespace back and stops nothing; for the unit files, reinstall the package and `systemctl daemon-reload` first, as a restart of the old unit tears the gateway down again; `systemctl enable pco-netns`. |
| `leg <id>` | The leg of the network on this node is not up, with why: its bridge is absent, another machine holds its address, its card could not be added (fails); it is up with a problem, as `no carrier, DAD not run` on a bridge whose uplink has no carrier (warns). | `pco network show <id>`; `pco managed status` for the leg of `pconet`. |
| `networks` | 13 or more of the 15 networks a node reaches are attached, the managed network among them (warns). | Detach a network that serves nothing; `pco network list` shows what each serves. |
| `service pool` | 80 per cent of the service addresses are given out (warns), or 95 per cent (fails). An address is never given twice, released ones count, and once none is left a new target behind a leg is held. It warns when the pool cannot be read, or reads as holding no address. | Find the targets behind a leg whose addresses change often: a target keeps its service address while its address stays. |
| `managed` | One finding for each of these that holds. The managed network cannot run on this node, with the reason of its problem line (fails); its leg is not up (fails). It warns while automatic attaches are stopped, with the line that says why; while the tag `cf-tunnel-managed` is not registered as admin-only under `per-guest` consent, so that it gives no guest consent and only approvals do; and while fewer than 10 addresses of `managed.subnet` are free. | What the reason says; add the tag to `registered-tags` in the options of the datacenter; detach the guests that no longer need a card. |
| `vnet` | The vnet `pconet` is not as pco made it, with what differs (fails): no card is added until it is. Changes of others to the SDN wait to be applied (warns): the next apply, on any node and pco's own among them, applies them along. It warns when those changes cannot be read; in the appliance, a token that may not read them leaves the check ok, with a note, since the vnet itself is still checked. | Put the vnet back: zone `pco` of type `simple`, `isolate-ports` on on a host and off in the appliance, no subnet. Apply or undo the other changes in Datacenter > SDN. |
| `pconet` | IPv6 is on on the bridge `pconet` of a host, which gives the node a link-local address the managed guests reach (fails), or it cannot be read (warns). | `systemctl restart pco-netns` turns it off again. |
| `dhcp` | The DHCP server of the managed guests is not bound (fails): a virtual machine on `pconet` gets no address. It warns while the server has not said how it is. When fine, it counts since the start the requests from MACs without a lease, which it does not answer, and the forged requests it dropped: those that named another card's MAC as the client while the frame came from the guest's own card. The journal names both MACs of a forged request, once a minute. That catches only the sloppy kind of forgery; a guest that forges the frame's source MAC as well is not caught here but at the leg's ingress, which lets through only the MAC and address pairs of the leases, and in the appliance by the MAC filter of the guest firewall. | `journalctl -u pco` says why; `systemctl restart pco`. A forged request: find the guest whose card has the source MAC. |
| `managed guest <owner>` | A guest's card is not plainly attached (warns): `pending-reboot` (a virtual machine without network hotplug takes the card at its next restart), `awaiting-dhcp`, `needs-grant` with the `pveum` line, `detaching` with the time it goes and why, `detach-pending` (the card goes once the guest's configuration no longer lists it, after a restart), `attached (unverified)` with the error of the last cycle. A guest that waits for its card on a grant, or on the confirmation that pco may turn its firewall on (`needs-firewall-confirmation`, which names the other card it filters), warns too. A guest that moved to another node fails: it keeps its card and its address, and the managed network does not reach it there. | The `pveum` line; `pco managed approve <owner> --accept-firewall-impact`, or `managed.acceptFirewallImpact` for every guest; restart the guest; give its consent back before a detach; migrate it back. |
| `foreign card <owner>` | A card on `pconet` that no attachment explains is still there: pco may not change the guest and only reported it (fails), or set it link down (warns). | Take the card off the guest; with consent, pco replaces it by a card of its own. |
| `filtered pconet` | In the appliance: the guests on `pconet` are not proven isolated by their firewall, with the first three reasons (fails); the `filtered` level is not offered on the node's firewall backend (warns). Their routes are proven at `observed` meanwhile. It warns before the first cycle judged it. | `pco managed status` lists every reason; `nftables: 0` in the firewall options of the node. |
| `managed firewall` | In the appliance: whether the node can isolate the managed guests. It fails when it cannot: its firewall runs on the nftables backend, the datacenter firewall is off, or `pve-firewall` does not run on the node. Nothing is attached until it can, the cards that exist stay, and their routes are proven at `observed`. It does not read the rules in force in the node's chains, which the appliance cannot see: `filtered pconet` says what the cycle proved of each guest. The check `firewall` above only says what is on. | What the line names, on the node. |
| `pool grant`, `pool grant <owner>` | In the appliance with a pool grant, a note (ok) for each guest of the pool without consent for the managed network: pco's token can change its cards and its firewall, and pco changes none of it without consent. A guest that is attached or waits, carries the registered tag `cf-tunnel-managed`, or carries the gate tag under `managed.consent` `auto` has consent and gets no note. It warns when the guests cannot be read. | Nothing, if the pool holds what pco may change; otherwise move the guest out of the pool. |

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
| `the gateway could not be programmed; no tunnel configuration is written until it is` | See [the gateway and service addresses](#the-gateway-and-service-addresses). |

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
| `unknown` | `leader.json` could not be read or used. Once a cycle reads it in order again, the verdict is `ok` until Cloudflare says otherwise, also while the cycle holds for another reason, as for a missing credential. | `pco setup`, or `pco setup --recover`. In the appliance, the problem line names the command; after a start of the container the verdict is `unknown` for the one cycle that draws the new epoch. |
| `behind` | The state of the appliance is older than the last write at Cloudflare. | `pco appliance recover`. |
