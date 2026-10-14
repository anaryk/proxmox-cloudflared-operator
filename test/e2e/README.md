# End-to-end suite

The suite runs pco as it is installed on a Proxmox VE node: the package, `pco setup`,
the daemon under systemd, real containers, the egress filter in nftables, and
`pco uninstall` at the end. Cloudflare is a fake served by the suite itself on a
loopback port, or the real API with a zone of yours.

It changes the node it runs on, so run it on a test node only.

It runs in one of four profiles: `host` (the default), pco installed on the node as
described below, or `appliance`, pco in a container of the node as
`pco appliance install` makes it; [The appliance profile](#the-appliance-profile) says
what that one needs and does. `-profile appliance` on the command line or
`PCO_E2E_PROFILE=appliance` chooses it. The profiles `cluster` and `appliance-cluster`
run on the nodes of a Proxmox VE cluster instead: [The cluster profile](#the-cluster-profile)
and [The appliance on a cluster](#the-appliance-on-a-cluster) say what they need and do.

## What it needs

- Proxmox VE 8.4 or later, run as root. Without root, `pct`, `qm` or pco it skips.
- A node of its own: not in a cluster.
- pco installed from a package, and not set up: no `/etc/pve/pco`.
- The bridge `vmbr1`, with no port but those of guests, so that nothing the suite puts
  on it leaves the node, and without the address `10.77.0.1`.
- No guest, container or VM, with an id from 9100 to 9199 or from 9350 to 9399, and
  no guest tagged `cf-tunnel`: the daemon would serve it too, and against the real
  API publish it.
- The Alpine template `local:vztmpl/alpine-3.24-default_20260714_amd64.tar.xz` and
  the storage `local-lvm`; `PCO_E2E_TEMPLATE` and `PCO_E2E_STORAGE` name others.
- `pvestatd` running, or free to be started for the run: it tells the cluster's
  resource listing whether a guest runs, and without it the daemon serves no guest
  it has never seen running.
- For the networking scenarios: no bridge `pcot1`, `pcot2`, `pcotv` or `pcoti` (the
  suite makes them for the run), no SDN zone `pco` and no vnet `pconet` (pco makes
  them for the managed network, and takes them away with the install), and
  `pve-firewall` either running or free to be started for the run. `conntrack` is
  read where it is installed. The QEMU parts of the managed network need a Debian
  cloud image on the node, named by `PCO_E2E_CLOUD_IMAGE` (a `genericcloud` qcow2
  of Debian 12 or 13), with `ssh-keygen` on the node; without it they are skipped.
  `PCO_E2E_VM_STORAGE` names the storage of the machines' disks when it is not
  `PCO_E2E_STORAGE`.
- `/usr/bin/cloudflared` against the real API. With the fake the suite does without:
  setup then runs with `--skip-cloudflared` rather than install it, and the
  connectors' units are enabled but cannot start, which with the fake's token they
  could not do anyway.

The suite refuses a node that does not have all of this. It points at `cleanup.sh` only
when what is in the way was left by a run of its own.

## What it does to the node

- Sets pco up, and uninstalls it at the end with `--purge-cloudflare`.
- Starts `pvestatd` when it is not running, and stops it again at the end.
- Makes containers 9100 to 9199 named `e2e-<vmid>`, with `10.77.0.<vmid - 9000>/24`
  on `vmbr1`, and destroys them at the end.
- Gives `vmbr1` the address `10.77.0.1/24`, and brings it up when it is down; both are
  undone at the end.
- With the fake, writes the drop-in `/etc/systemd/system/pco.service.d/e2e.conf`, which
  points the daemon at it with `PCO_CLOUDFLARE_API_URL`, and passes the same variable to
  `pco setup` and `pco uninstall`. That variable is for tests only: pco accepts it for an
  `http` or `https` URL of a loopback address or `localhost`, warns when it starts, and
  `pco status` shows it as a problem for as long as it is set.
- Kills `pco.service` with SIGKILL, and stops `pveproxy` for 35 seconds (S13).
- Runs `nft flush ruleset` (S14), which takes the tables of others as well. It saves
  them first, and loads those that do not come back by themselves as they were.
- For the networking scenarios: makes the runtime bridges `pcot1`, `pcot2`, `pcotv`
  and `pcoti` with `ip link add` (never in `/etc/network/interfaces`) and removes
  them at the end, with a bond without slaves, `pcot1p0`, as the port of `pcot1`
  that stands for a NIC (Proxmox makes the bridge of a tagged guest from the
  physical ports of its bridge, and refuses a bridge that has none); makes guests
  9350 to 9399 on them, containers and, with a cloud
  image, virtual machines; attaches networks to them and detaches them again, which
  enables `pco-netns.service` and brings up the gateway namespace `pco`; enables the
  managed network (`pco network managed enable`: the SDN zone `pco` and vnet
  `pconet`, applied with an SDN reload) and lets the daemon put cards on its guests,
  with `managed.detachGrace` at one minute for the run; adds `cf-tunnel-managed` to
  the registered tags and takes them away for a while, putting them back as they
  were before pco is uninstalled; sets `net.bridge.bridge-nf-call-iptables=1`,
  `net.ipv4.ip_forward=1` and `net.ipv4.conf.all.rp_filter=0` and puts them back,
  and `rp_filter` of `pconet` for a while in S22; starts `pve-firewall` when it is not
  running and stops it again; restarts `pco-netns` and `pco`, and stops `pco` while
  it clones a guest of the managed network; reinstalls the
  package when `PCO_E2E_DEB` names it (or one `pco_*_amd64.deb` is in the working
  directory); runs `ifreload -a` and `pvesh set /cluster/sdn`.

It records all of this in `/var/lib/pco-e2e`: the install it set up and the mode of the
run (`owned`), the address and the state of the bridge, the tables it saved, and, against
the real API, the record S10 makes. The directory stays after the run, so that
`cleanup.sh` knows which install and which package are the suite's.

## The cleanup

The suite undoes everything in its own cleanup, also when a scenario fails, except the
package. `cleanup.sh` finishes that, and undoes what a run that was cut off left:

- it starts `pveproxy` when it is not running, and loads the saved tables that are
  missing;
- it uninstalls pco only when `/var/lib/pco-e2e/owned` names the install that is set
  up: with `--purge-cloudflare` after a run against the real API, while the credential
  is still stored, and with `--keep-cloudflare` after one against the fake, whose
  objects went with the suite's process;
- it puts the registered tags back, destroys the guests 9350 to 9399 named `e2e-*`
  before the uninstall (a card left on `pconet` would keep its zone), and uninstalls
  with `--detach-managed` when a run enabled the managed network;
- it destroys the containers 9100 to 9199 named `e2e-*`, removes the drop-in, the
  address and the token file, and takes the bridge down when the suite brought it up;
- it puts the kernel parameters back, stops `pve-firewall` and `pvestatd` when the
  suite started them, and removes the runtime bridges the suite made;
- then it purges the package and removes the user `pco-connector`, and says what
  should be gone and is not: the gateway namespace, its host tables and the vnet
  `pconet` among it.

An install the suite did not record is left alone, with the package, the connectors and
the egress table. So is one set up by a run that was cut off in `pco setup` itself,
before the suite could record it: check it with `pco status`, and remove it with
`pco uninstall`. A record S10 made in a real zone and could not delete is named, to be
deleted by hand. `cleanup.sh` exits 1 when something is left, and keeps
`/var/lib/pco-e2e` for its next run.

## Running it

Build the package and the suite on a workstation:

    make snapshot e2e-binaries

Copy `dist/pco_*_amd64.deb`, `bin/e2e.test` and `test/e2e/cleanup.sh` to the node, then
on the node:

    apt install ./pco_*_amd64.deb
    ./e2e.test -test.v -test.timeout 90m 2>&1 | tee e2e.log
    ./cleanup.sh

On a node whose containers live on the directory storage `local`, put
`PCO_E2E_STORAGE=local` in front of `./e2e.test`.

`-test.run 'TestEndToEnd/S1_'` runs one scenario. S11 expects the install that setup
leaves, so it runs first or not at all; the others leave observe-only mode themselves.
The networking scenarios make what they build on when it is missing: S19, S20 and
S23 attach the networks of S17 and S19, S22 and S23 enable the managed network of
S21. `-test.run 'TestEndToEnd/S2[1-3]'` runs the managed network alone.

For the networking scenarios, put the package of the build under test beside the
binary (or name it with `PCO_E2E_DEB`) so that S23 reinstalls it under traffic, and
name a Debian cloud image with `PCO_E2E_CLOUD_IMAGE` so that S21 runs its machines:

    PCO_E2E_CLOUD_IMAGE=/var/lib/vz/template/iso/debian-13-genericcloud-amd64.qcow2 \
    PCO_E2E_DEB=./pco_0.9.0_amd64.deb ./e2e.test -test.v -test.timeout 150m 2>&1 | tee e2e.log

A full run with them takes about two hours.

Against the real Cloudflare API, put a token as described in
[Cloudflare token](../../docs/cloudflare-token.md) in a file only root can read, and
name it and a zone the token may change:

    PCO_E2E_CF_TOKEN_FILE=/root/cf-token PCO_E2E_ZONE=example.com ./e2e.test -test.v -test.timeout 90m

The token is never taken from the environment or the command line, and no command the
suite runs sees a variable that begins with `PCO_E2E_CF_TOKEN`. The suite copies it for
`pco setup` into `/var/lib/pco-e2e/cf-token`, which it removes as soon as setup has
run; `cleanup.sh` removes it too. It never prints the token, nor the tokens of the
tunnels.

The suite then publishes names below `e2e-*.<zone>`, fetches one of them through
Cloudflare, and deletes everything again with the purge, which it tries three times
before it names what is left. It reads the zone only by name or by the comment of its
own records, never in full, and asks Cloudflare no more than every 3 seconds while it
waits. The scenarios that need the fake (a token that loses a permission, an outage of
the API) are skipped.

## Scenarios

| Id | What is checked |
|---|---|
| S11 | A new install only observes: the plan holds every action, nothing is made at Cloudflare and no connector runs, until `pco apply`. Six records of six going at once are held by the mass-delete guard until `pco apply --confirm-deletes`. |
| S1 | One route goes active: the rule in the tunnel, the record, the connector unit enabled with the token of the tunnel, the target in the egress set. With the fake the connector never connects and is not ready; with Cloudflare it is, and the name answers with the container's body. |
| S2 | Two hostnames on two ports of one guest. |
| S3 | Three hostnames to one port. |
| S5 | A wildcard and an exact name below it: the exact rule comes first. |
| S6 | A container with a static `ip=`, and one with `ip=manual` whose address only the running container reports. |
| S7 | Stop (503 rule, record kept, target out of the egress set), start, an edited name (the old record goes after the grace), the tag removed, the guest destroyed. |
| S8 | A clone with the notes: conflict, and the original keeps serving. |
| S10 | A record of someone else in the way: conflict, `pco adopt`. With the fake the adoption waits for a ready connector, and the event says so. |
| S12 | The token loses DNS write: a problem, the record is not made, nothing is deleted; it is made once the permission is back. |
| S13 | The daemon killed with SIGKILL in a cycle (with the fake, while the cycle waits for its read of the tunnel's configuration), `pveproxy` stopped and the API down, each for longer than the grace: the daemon holds and changes nothing, and everything is as before afterwards. The records compared are the run's, without the time of their last change. |
| S14 | `nft flush ruleset`: the egress table comes back with its targets, with an event. Tables of others that do not come back by themselves are loaded again as they were. |
| S15 | A published container takes another MAC, with its link up: the target stays in the egress set until the node sees the new MAC, leaves it within 2 seconds, the event says the MAC moved, and it comes back with the MAC. |
| S9 | A container that answers for the node's address and for another guest's, and names both: never published to either: no record for its names, which have no rule but the one that answers 503, no rule to the node, and the other guest's address serves that guest's name only. |
| S17 | A guest on a bridge the node has no address on, `pcot1`, is not served until the network is attached; then its route is active through `attach:pcot1` at `port`, the connectors dial a service address of `198.18.0.0/16`, that address answers the guest's body from the node, the egress set holds it and never the guest's real address, the namespace translates it, and the allocation is an event. The route withdrawn: the service address leaves the set and is released, the real address still not in it. The route back: a later address, never the released one again. |
| S18 | A tag on a bridge that filters VLANs: the leg's port carries the tag as its PVID and nothing else, the route is active through `attach:pcotv.20`. A tag on a bridge that does not filter VLANs: the leg sits on the bridge Proxmox makes for it, `pcot1v30`; with the tagged guest stopped and that bridge gone the leg is down with its problem, and when the guest starts the leg is back on it without anyone asking. Both detached: the legs are gone and their service addresses released. |
| S19 | Two attached networks with the same prefix and the same guest address, with `pve-firewall` running and `bridge-nf-call-iptables` on: two service addresses, 500 requests in parallel each answered by the right guest, the source ports in the host's conntrack in the two legs' own blocks. One guest's MAC changes: only its service address leaves the set, the other keeps serving; back, it keeps the service address it had. A Direct route beside them: its MAC change is seen within 2 seconds, as in S15. |
| S20 | `ifreload -a` and `pvesh set /cluster/sdn` with legs up and under requests: no request fails, the legs stay on their bridges, the namespace is as pco left it, the host's routes of the service prefix stay. The second network detached: its leg and service address go, the first keeps serving. |
| S21 | The managed network: `pco network managed enable` makes the vnet with isolated ports and no subnet, the leg is up and DHCP listens. A container tagged `cf-tunnel-managed` gets one card on `pconet` with its lease, `firewall=0` and the lease as its static address, nothing else of its configuration changes, the card is up inside, the route is active through `managed` at `port`, the node's forwarding table holds the lease MAC sticky on the guest's port, the guard keeps the card to it, and the namespace pins the address. With a cloud image: a VM gets a card, its DHCP client gets the lease, the route is active, and a renewal sent unicast to the server is answered unicast; a VM without network hotplug waits with `pending-reboot` until it is restarted. A clone of the container inherits the tag, its copied card is replaced by one of its own with its own lease, and its hostname is in conflict while the original keeps serving; an untagged clone's copied card is set link down. Under automatic consent a guest with only the gate tag gets a card, one with `cf-tunnel-managed=off` in its notes never does, and the marker on an attached guest starts the grace, which taking it away ends. The registered tags cleared for longer than the grace: automatic attaches stop, no card goes, and they resume when the tags are back. A restart of the daemon changes no card, no service address, no route and writes nothing in its first cycle. |
| S22 | Isolation of the managed network on a host: a managed guest reaches neither another managed guest nor the leg's other ports; not the node over IPv4, after ARP for its address; not the LAN or the Internet through the node as its gateway with forwarding on (`routed_from_pconet` counts it); the managed bridge has no IPv6 address; a guest that forwards to the service prefix through the node gets nothing into the transit link (`to_transit` counts it); a forged address, then a forged MAC, from another managed guest reach nothing (`dropped_spoofed` counts both), the owner's sticky entry stays and it keeps serving; a card put on `pconet` by hand is found within a cycle and set link down. |
| S23 | `systemctl restart pco-netns` under requests through an attached and a managed network: no request fails, the namespace's ruleset, its service addresses, the host's routes of the service prefix and the active routes are as before. The same for a reinstall of the package, after which the first cycle writes nothing at Cloudflare. The reboot of the node is a step of the lab, below. |

A target counts as absent from the egress set only while `pco egress show` exits 0 and
says the filter is on: a table that is not loaded has no target either.

The grace is 30 seconds and the poll interval 5 seconds for the run; the detach grace
of the managed network is one minute. A run of S1 to S15 takes about half an hour,
the networking scenarios about an hour more.

In the appliance profile the same scenarios run against the appliance, with its guests
in its own range (see below), at the level `observed` where the host proves `port`;
S14 flushes the appliance's ruleset and waits for both of pco's tables there, and finds
no other table; S15 counts the 2 seconds from the appliance's neighbour table; S9's
attacker names the node's address on the bridge, which only the run gives it, and the
appliance's, and takes only the other guest's.

## The appliance profile

The appliance profile installs the appliance from the package and the template of a
build, with the installer of that package unpacked into the run's directory, as
`install.sh` hands over to it, and runs every pco command inside it through
`pct exec`. pco there accepts the Cloudflare API of tests only on a loopback address, so
the fake listens on the node's address on `vmbr1` and a relay of the suite, `e2e-relay`,
listens on the appliance's `127.0.0.1:8787` and forwards to it. The settings are changed
as the host's are, in pco's own store code: the file is pulled into a scratch store on
the node, changed there, pushed back beside the one in the appliance and renamed over it.

### What it needs

- Proxmox VE 8.4 or later, run as root, on a node of its own, with `vzdump` to the
  storage `local` and `gpg` (for A-S17).
- pco not set up on the node, and neither the user `pco@pve` nor the pool `pco`.
- No guest with an id of `PCO_E2E_VMIDS` (default `9250-9299`, fifty ids). The appliance
  takes the first of them, or `PCO_E2E_APPLIANCE_VMID`: an id of the range at the offset
  0, 4, 9, 10 or 19, which no scenario's guest takes.
- The bridge `vmbr1` with no port but those of guests and without `10.92.0.100`; no
  bridge `pcote1`, `pcot1`, `pcot2` or `pcoti`.
- A storage that snapshots a running container, clones from a snapshot and makes
  snapshot backups, `PCO_E2E_STORAGE` (default `pcotest-zfs`), and the Alpine template
  of the host profile on it or named with `PCO_E2E_TEMPLATE`.
- In the working directory, or named: the build's `pco_<version>_amd64.deb`
  (`PCO_E2E_DEB`), its `pco-appliance_<version>_amd64.tar.zst` (`PCO_E2E_APPLIANCE_TEMPLATE`),
  its `checksums.txt` if there is one (`PCO_E2E_CHECKSUMS`), and `e2e-relay`
  (`PCO_E2E_RELAY`). A-S17 runs when `PCO_E2E_UPGRADE_DEB` names the package of a
  later build.
- Against the real API, besides the token and the zone: `PCO_E2E_APPLIANCE_BRIDGE`, a
  bridge with an uplink for the appliance's `net0` by DHCP, through which it reaches
  Cloudflare and the API; the guests' card is then `net1` on `vmbr1`, and the scenarios
  that need the fake are skipped or leave out what only the fake shows.

### What it does to the node

- Gives `vmbr1` the address `10.92.0.100/24` for the run, serves the fake there, and
  installs the appliance at `10.92.0.150/24`, gateway `10.92.0.199`; the guest at the
  offset n of the range is at `10.92.0.<150 + n>`.
- In the appliance: the relay as the enabled unit `e2e-relay.service`, the drop-in
  `/etc/systemd/system/pco.service.d/e2e.conf` with `PCO_CLOUDFLARE_API_URL`, the
  credential of the run, and the settings of the run.
- Makes the test principals `pcotest-e2e@pve` (with the tokens `np` and `ps` for a
  while), role `PCOTESTNET` and pool `pcotest-e2e`, and grants and takes back ACL lines
  for them, on the appliance among others, and the lines pco asks for the managed guests.
- Snapshots, clones, reboots, rolls back, backs up and restores the appliance; the
  backups go to `/var/lib/vz/dump` and are removed again.
- Makes the runtime bridge `pcote1` and, for the networking scenarios, `pcot1`, `pcot2`
  and `pcoti`; grants the appliance those networks and makes its managed network
  (`pco appliance managed enable`: the SDN zone `pco` and vnet `pconet`, applied with an
  SDN reload). For that it turns the datacenter firewall on with the node's own rules
  off, so that the node stays reachable as before, sets the node's `nftables` option, and
  starts `pve-firewall` when it is not running; all of it is put back.
- Uninstalls the appliance with `--purge-cloudflare` in A-S15, or in its cleanup.

It records all of this under `/var/lib/pco-e2e` (`appliance`, `principals`,
`firewall-options` and the files of the host profile it shares) and the registered tags
before the install in `/run/pco-e2e-registered-tags`. With the file `appliance` there,
`cleanup.sh` works in its appliance mode: it destroys the containers of the range that
are the appliance, a copy of it (by the mark in their description) or the suite's
guests (`e2e-*`, `pco-clone`), snapshots first and protection off; removes the tokens
`pco@pve!vm<id>` of the range, then `pco@pve`, `PCO` (and `PCOManaged`, `PCOSDN`) and
pool `pco` when nothing else names them; the test principals; puts back the registered
tags and the firewall options; and removes the bridges, the node's address, the run's
backups and `/root/.pco-appliance-install`. It reports a vnet `pconet` it finds, and
leaves anything else.

### Running it

Build the package, the template and the suite on a workstation (the template needs
Linux, see `packaging/appliance/README.md`, or take it from the CI run of the commit):

    make snapshot template e2e-binaries

Copy `dist/pco_*_amd64.deb`, `build/appliance/pco-appliance_*_amd64.tar.zst`,
`bin/e2e.test`, `bin/e2e-relay` and `test/e2e/cleanup.sh` into one directory on the
node, then there:

    PCO_E2E_PROFILE=appliance ./e2e.test -test.v -test.timeout 240m 2>&1 | tee e2e-appliance.log
    PCO_E2E_PROFILE=appliance ./cleanup.sh

Nothing is installed on the node. A-S1 needs the install the suite makes and runs
first; A-S15 uninstalls it and runs last. `-test.run 'TestEndToEnd/A-S1_|TestEndToEnd/S1_'`
runs a part.

### Scenarios of the appliance

The run is A-S1, A-S2, A-S3, then the host's scenarios from S11 to S9 as above, then
A-S4, A-S5, A-S6, A-S13, A-S14, A-S12, A-S10, A-S11, A-S7, A-S8, A-S16, A-S17, A-S9,
the networking scenarios in the appliance, and A-S15.

| Id | What is checked |
|---|---|
| A-S1 | The install without a Cloudflare token: the container's configuration value by value, role `PCO` with exactly the release's privileges, `pco@pve`, its token with privilege separation, their ACL lines on `/`, pool `pco` with the appliance alone, the gate tag registered; nothing under `/etc` or `/usr` of the node newer than the run, no package or unit file changed, no journal of the installer left; inside the profile, the volume and its marker, the mount source of the appliance's own VMID, no bootstrap, the CA, the install record (VMID, node, MAC, endpoint), `observed` and observe-only, the node's address saved, `pco status`, `nftables.service` masked, `pco net show` with nothing leaked, no connector. |
| A-S2 | The relay and the drop-in, the credential added inside and its file gone, a complete cycle, the override's problem line the only one besides observe-only. |
| A-S3 | A route on `vmbr1` held until the segment is acknowledged, then active at `observed` within two cycles with the event of the first pin; nothing written. |
| A-S4 | Another guest's MAC and address: held with `MAC changed`, the address never served, until approved; back to its own MAC and to another address under it: served within two cycles without an approval and without the event. |
| A-S5 | `VM.Config.Network` of a test user on a guest, directly and through a pool: the route waits while it holds; `PVEVMUser` as the control. `PVEVMUser` on the appliance: the connectors stop, the flag goes, the egress set empties, writes are held, the problem line gives the exact `pveum` line, which brings it back. A token without privilege separation answered on its user, one with it on itself. |
| A-S6 | A guest at the appliance's gateway waits until approved with `--allow-address`. |
| A-S13 | A card of the appliance on a second segment: pending for at most a minute, no hold; a guest there waits for the segment; the card removed: the route goes, the identity stays. |
| A-S14 | `nftables.service` unmasked, restarted and stopped: both of pco's tables back with their content and an event each, every time. |
| A-S12 | `pco doctor --json`: the checks that are not ok are exactly those an appliance without an uplink fails. |
| A-S10 | A guest outside pool `pco` with the appliance's MAC: a tenant, its route rejected, the appliance proven still, S1 never withdrawn, nothing written for the tenant; all served again once it is gone. |
| A-S11 | A reboot: `pco-net` and `pco-egress` before any connector, no connector before the identity passed, nothing leaked, the epoch one up, one configuration write per tunnel and nothing else; a restart of the daemon writes nothing and keeps the epoch. |
| A-S7 | A clone from a snapshot in pool `pco`, with a MAC of its own: a copy within 30 seconds of its start, for three cycles no connector, no flag, no target, the cloned `leader.json`; the original's generation alone reaches Cloudflare, and a route added meanwhile is published. |
| A-S8 | The clone with the appliance's MAC: the original names it as a copy and holds its writes until it is gone, then publishes what waited. |
| A-S16 | A rollback after a reboot: held as `behind` with the line naming `pco appliance recover`, doctor says the same, nothing written, recover and apply serve again above Cloudflare. A rollback without a start in between: never `behind`, a write verified, the event and doctor's note, the approval made after the snapshot gone. |
| A-S17 | `pco upgrade pco` from a release the suite signs and serves: the warning, the new version, both packages held, the old package kept; `--rollback`; a `checksums.txt` that its signature does not match refused before a package is fetched. |
| A-S9 | A backup restored to another VMID: `pco.service` exits 78 without a restart loop, `pco status` names the repair, nothing written, and the repair refuses the copy beside its original. Restored over the appliance: refused the same way until `pco appliance repair --recover`, then served again above Cloudflare. |
| S17, S19 | In the appliance: the network granted on the node, the leg a card of the container, the guests reached through their service addresses from inside the appliance; with one prefix twice, each hostname answered by its own guest, and one guest's MAC change takes only its service address out. |
| S21 | In the appliance: the managed network made on the node and taken in inside, a guest's card once the line pco prints ran, at the level `filtered`; the nftables backend, the datacenter firewall off and `pve-firewall` stopped each refused for a while, with nothing detached and the route at `observed`, and back; a guest as the GUI makes it waits for the confirmation of the firewall impact, and its own card answers after. |
| S22 | In the appliance: pco's rules in the guest's chain of `iptables-save`, guest to guest nothing, a card put on `pconet` by hand drops the level to `observed` and is reported, and back. |
| A-S15 | The uninstall: no container, token, ACL line, user, role or pool of pco left, the registered tags as before, nothing at Cloudflare; with the rest the run made gone, the node's packages, unit files, ruleset, `/etc` and `/usr` as before. |

## After a reboot of the node

The suite cannot reboot the node it runs on, and the fake Cloudflare lives in its
process. The reboot is checked by hand, with an install made as the suite makes it
(`pco setup`, `pco network attach pcot1 --address 10.50.0.250/24`, `pco network managed
enable`, a tagged container behind each) against a Cloudflare that outlives the reboot:
the real API, or `hack/cffake` on another machine forwarded to the node's loopback
with `ssh -R`. With the networks attached and the managed network enabled, note
`pco netns status`, the service addresses (`ls /etc/pve/pco/svcaddrs`), the cards
(`pct config <vmid>`) and `pco routes`, then `reboot`. After it:

- `journalctl -b -o short-iso -u pco-netns -u pco -u pve-guests` shows the lines
  `leg leg<N> up ...` of `pco netns up` and `Finished pco-netns.service` before
  `Starting pve-guests.service`, and the line of the DHCP server listening before the
  managed guests started;
- `pco netns status` exits 0; `ls /etc/pve/pco/svcaddrs` and the cards are as noted;
- after the first cycle `pco routes` is as noted, with the same service addresses, and
  `pco events --kind network --since <boot>` has no allocation;
- against the real API, `https://<hostname>/` answers through the leg as before.

## The cluster profile

The cluster profile, `TestCluster`, sets pco up on every node of a cluster of three nodes
or more, and runs the scenarios of a cluster against that one install: placement and the
moves of routes, the controller's failover, drain, forget and the rotation after a
compromised node, networking on every node, an upgrade node by node, `pco doctor
--cluster`, and the uninstall of a node and of the whole cluster. It runs on one node of
the cluster, the driver, and reaches the others with ssh as root, as the nodes of a
cluster reach each other. The driver is never powered off, rebooted, suspended or cut off:
a scenario that does so to the controller first hands the lease to another node.

The fake Cloudflare stays in the suite's process on the driver and listens on the
driver's address in the cluster (the `ip` of `/etc/pve/.members`). pco accepts the API
of tests on a loopback address only, so every node, the driver too, gets the suite's
relay as the unit `e2e-relay.service` on `127.0.0.1:8787`, which forwards to the fake and
outlives a reboot of the node, and the drop-in `pco.service.d/e2e.conf` that points the
daemon at it. The fake logs which node each request came from, by the address the relay
of that node dials from, which is how the failover scenarios tell a write of the old
controller from one of the new.

With the fake, no connector connects, so the suite asks what Cloudflare would answer
from what the fake holds: a hostname is served while its record leads to a tunnel whose
rule for it sends it to a service and not to a status. A visitor asks that every second
for the hostnames a scenario keeps serving, and the scenario fails on any answer that
would not have been served. What a connector on a node would reach, it fetches on that
node with `curl`. Against the real API the visitor fetches the hostnames through
Cloudflare instead.

### What the cluster profile needs

- Proxmox VE 8.4 or later on three nodes or more of one cluster, every node online, the
  suite run as root on one of them; root on that node reaches every other node with ssh,
  as a cluster sets it up.
- pco installed from the same package on every node, and not set up: no `/etc/pve/pco`.
- An SDN vnet that spans the nodes for the guests, `labx` or the one
  `PCO_E2E_CLUSTER_BRIDGE` names, with an IPv4 address of each node in one /24 of it. The
  guests take the addresses 100 to 149 of that /24, which nothing may answer.
- No guest with an id from 9500 to 9549 on any node, and no guest tagged `cf-tunnel`.
- The Alpine template and the storage of the host profile on every node.
- For the scenarios that power a node off, suspend it or take it out of the cluster: a
  command that does so, named by `PCO_E2E_POWER`. The suite runs it on the driver as
  `<command> off|on|suspend|resume <node>`; for nodes that are virtual machines of a host
  the suite cannot reach, it is the way to that host. Without it those scenarios are
  skipped.
- `e2e-relay` beside the suite, or named by `PCO_E2E_RELAY`.

### Running the cluster profile

Build the package and the suite as for the host profile, install the package on every
node, copy `bin/e2e.test`, `bin/e2e-relay` and `test/e2e/cleanup-cluster.sh` to the
driver, then there:

    PCO_E2E_PROFILE=cluster PCO_E2E_POWER=/root/power ./e2e.test -test.v -test.run TestCluster \
        -test.timeout 600m 2>&1 | tee e2e-cluster.log
    ./cleanup-cluster.sh

A full run takes several hours: ten power losses of the controller, half an hour without
a controller and the waits of ten minutes of coverage and blocks are most of it.
`-test.run 'TestCluster/C2_'` runs one scenario; the scenarios make the guests they look
at when the run does not have them yet. More of the environment:

| Variable | What it does |
|---|---|
| `PCO_E2E_POWER_RUNS` | how often C21 powers the controller's node off, 10 by default |
| `PCO_E2E_FLAP` | how long C4 restarts an agent every 50 seconds, `10m` by default |
| `PCO_E2E_ABSENT` | how long C20 runs without a controller, `30m` by default |
| `PCO_E2E_UPGRADE_DEB` | the package of a later build, which C26 installs node by node; without it C26 is skipped |
| `PCO_E2E_CLOUD_IMAGE` | the cloud image of the host profile, for the machine C23 migrates; without it C23 is skipped |
| `PCO_E2E_COMPROMISE` | a node C25 takes out of the cluster for good with `pvecm delnode`, to rotate every secret after it; without it C25 checks only the refusal. The node is not set up again: make a new one afterwards |
| `PCO_E2E_CF_NEW_TOKEN_FILE` | against the real API, the token C25 replaces the first one with |

Against the real API, name the token and the zone as for the host profile; every node
then needs `/usr/bin/cloudflared`. The suite publishes names below `e2e-*.<zone>` alone,
and purges the install's tunnels, the pinned ones too, at the end.

### What the cluster profile does to the nodes

- Sets pco up on the driver and has the other nodes join it (`pco setup --yes`, which adds
  the datacenter firewall rule of the agent channel and the ipset `pco-nodes`), and
  uninstalls it from every node at the end.
- Installs the relay and the drop-in on every node, and removes them at the end.
- Makes guests 9500 to 9549 named `e2e-<vmid>` on the nodes, migrates some of them between
  nodes, and destroys them at the end.
- Stops, restarts, kills (`SIGKILL`), stops with `SIGSTOP` and continues `pco.service`
  on the nodes; turns its watchdog off on one node for a while with the drop-in
  `pco.service.d/e2e-watchdog.conf`; reboots nodes; restarts `corosync` on every node at
  once.
- Loads the table `inet pco_e2e_cut` on a node for a while: it drops corosync's traffic
  (UDP 5405 to 5412) to take the node's quorum, or TCP 8644 to cut its agent off.
- Changes the registry with pco's own store code to let the nodes hold the lease again
  after C20, and puts back `leader.json` of an earlier generation in C19.
- Makes the runtime bridges `pcotc1` and `pcotci` on the nodes for C7 and C8, attaches
  the first as a network and enables the managed network (SDN zone `pco`, vnet `pconet`),
  registering `cf-tunnel-managed` until the end.
- With `PCO_E2E_UPGRADE_DEB`, installs that package on every node; the nodes keep it.
- With `PCO_E2E_COMPROMISE`, powers that node off and removes it from the cluster.

The tokens and keys the profiles of a cluster handle, the Cloudflare tokens, the tunnels'
run tokens, the join tokens and the agents' keys, never go into a command line: a file
the suite puts on a node goes through the standard input of the command that writes it,
with mode 0600, in a directory only root may enter, and is removed when the scenario is
done. Every line the profiles log is scrubbed of those values first, and a line that
carried one fails the run; `go test -tags e2e -run TestSecretsStayOutOfLogs ./test/e2e`
checks that scrubbing without a node.

It records the nodes, the install and the mode of the run in `/var/lib/pco-e2e/cluster` on
the driver. `cleanup-cluster.sh`, run on the driver, removes what a run that was cut off
left: it removes the cuts and continues a stopped daemon, uninstalls pco from every node
while the store still names the run's install, destroys the run's guests wherever they
are, removes the relays, the drop-ins, the runtime bridges and the files the run copied,
and puts the registered tags back. A node that does not answer keeps what the run left
on it, and the script exits 1 until it is run again with the node back.

### Scenarios of the cluster

The guests of most scenarios are one Alpine container on each of three nodes, `c-1` to
`c-3`, whose routes every node serves while every agent runs.

| Id | What is checked |
|---|---|
| C1 | Every node active with an index of its own, one controller with a valid lease, every agent following its epoch and hearing it; `pco node list` and `pco cluster status`; a node that is not the controller sends `pco sync` to it. |
| C2 | Each guest's route active on the shared tunnel, at `port` on its own node and at `port (attested)` by it on the others, in every node's egress set, reached from every node; every node covered. |
| C3 | An agent stopped for longer than the grace: the routes of the other nodes' guests are pinned to their nodes and their records follow, no record is deleted, the problem line and doctor say why; a guest migrated while pinned moves to the new node's tunnel and the rule in the old one goes five minutes later; the agent back, the routes are shared again after ten minutes. The visitor sees no failure. |
| C4 | An agent restarted every 50 seconds for ten minutes: no route moves more than once, and they are shared again afterwards. |
| C5 | `pco node drain`: the node leaves the shared tunnel's replicas, its guest's route is pinned to it; `undrain` brings it back. |
| C6 | A container with the MAC of a published guest started on another node: the guest's address leaves every node's egress set within two seconds of the cloning node's forwarding table learning the MAC, the block names that node, and holds for nine minutes after the clone is gone; `pco cluster unblock` ends it. |
| C7 | A bridge without an uplink attached as a network on every node: a segment of each node, legs up, two guests with the same address on two nodes each reached through its own node's service address and pinned to it, two mappings in `nodesvcaddrs`. |
| C8 | The managed network: a consenting guest on a node that is not the controller gets its card, its route is served through it from its node and pinned there. |
| C9 | The agent endpoint answers a registered node with its own certificate, drops another address of the node before TLS, and refuses a node's certificate from another node's address with `other-source`. |
| C10 | `pco doctor --cluster`: every agent answers, the checks of the cluster pass; an agent stopped fails `agent channel` and warns in `coverage`; TCP 8644 cut on a node fails its `agent dial` and the controller's `agent channel`. |
| C11 | `pco cluster handoff --to`: the named node holds the lease within 30 seconds, one generation up, with its sentinel written. |
| C12 | The controller's daemon killed with `SIGKILL`: the same node finishes a cycle within 30 seconds with the same generation and nonce, no other node takes the lease, no configuration is written again. |
| C13 | The controller's node rebooted: another node finishes an enforcing cycle within 20 seconds. |
| C14 | An agent stopped, then the controller: the new controller says it waits for that agent, and no rule, record or tunnel is withdrawn for its first 90 seconds. |
| C15 | `corosync` restarted on every node at once: the controller keeps its epoch, no takeover hold, no sentinel written again. |
| C16 | The controller's daemon stopped with `SIGSTOP`: systemd's watchdog restarts it within 90 seconds and a restart, no other node takes the lease, the epoch stays. |
| C17 | The controller's node cut off from corosync: it shows the quorum line, writes nothing at Cloudflare later than five seconds after its `.members` says it is not quorate, and the quorate nodes take over. |
| C18 | The controller stopped with `SIGSTOP` past the lock's expiry, with the watchdog off: no request of it reaches Cloudflare after the new epoch's first write, also after `SIGCONT`. |
| C19 | `leader.json` of two generations back put back: a node says it is behind, nothing is written until `pco promote`, and after it and `pco apply` a sentinel above both is. |
| C20 | No node may hold the lease (`pco node set --controller-eligible=false`) for half an hour: nothing written at Cloudflare, every node keeps its egress set; a node rebooted in that time serves the shared routes after its boot. |
| C21 | The controller's node powered off, ten times: the new epoch's sentinel within 150 seconds each time. |
| C22 | The controller's node suspended past the expiry and resumed: no request of it reaches Cloudflare later than five seconds after the resume, and the new controller's sentinel stands. |
| C23 | A machine on the driver migrated live under requests from every node: none fails from the other nodes, and those from the driver fail for three seconds at most. |
| C24 | A node powered off and forgotten: the shared tunnel's secret rotated, the remaining replicas run the new token, the node's pinned tunnel deleted, no hostname of the others fails; powered on, the node stands down as removed and runs no connector; then it joins again. |
| C25 | `pco node forget --compromised` is refused while the node is in the cluster, with the `pvecm delnode` line. With `PCO_E2E_COMPROMISE`: after `pvecm delnode`, a new Cloudflare token and `pco setup --repair`, the rotation finishes with every tunnel's secret, the CA and every remaining agent's certificate new, and a certificate of the old CA refused. |
| C26 | The package of a later build installed node by node, the controller last: every node active on the new build, the epoch kept while other nodes upgrade, `finish-upgrade` refused while nodes run the earlier build, no hostname fails. |
| C27 | One node uninstalled: it leaves the registry and is as it was before setup; then `pco uninstall --cluster` on the controller and on the others: nothing of the install at Cloudflare, every node as before, no install in the store. |

## The appliance on a cluster

The profile `appliance-cluster`, `TestApplianceCluster`, installs a controller container
on the driver and an agent container on two other nodes, from the package and the
template of a build, as the appliance profile does on one node. It needs what the
cluster profile needs except the package installed on the nodes, and what the appliance
profile needs: no user `pco@pve` and no pool `pco` in the cluster, the build's package,
template, checksums and relay in the working directory or named. The containers sit on
the bridge of the guests at the offsets 40 (the controller) and 43 and 44 (the agents),
and the fake listens on the driver's address there.

    PCO_E2E_PROFILE=appliance-cluster ./e2e.test -test.v -test.run TestApplianceCluster \
        -test.timeout 120m 2>&1 | tee e2e-appliance-cluster.log

After a run that was cut off, run `PCO_E2E_PROFILE=appliance ./cleanup.sh` on each node
with an agent container first, then on the driver: each node has the record of its
container under `/var/lib/pco-e2e`.

| Id | What is checked |
|---|---|
| A-C1 | The controller container active in its own registry, enforcing, its agent endpoint on its own address only, its firewall rule for TCP 8644 from `pco-agents`. |
| A-C2 | An agent installed with `--join-file` waits as joining without connectors; admission with seven hex digits of its fingerprint and with a wrong prefix is refused, with twelve right ones it is active and hears the controller. |
| A-C3 | A guest on each agent's node, once the segment is acknowledged: active at `observed` on that node, pinned to its tunnel, no attestation of an appliance agent taken, the target in the agent's egress set. |
| A-C4 | The join tokens are in no argument of any process of the agent's node while it installs (read every second), in no journal of the nodes and the containers, no event and no doctor answer. |
| A-C5 | An agent's node forgotten, then an agent there installed with the used token of another node: refused, with a security event; with a token of its own it joins again. |

## Wording the suite depends on

Where pco exports the text, the suite uses it (`reconcile.HeldByGuard`). These it
matches as written, and a change of wording in pco has to change them too:

- `observe` in the held reason of an action, and `observe mode` in `pco plan`;
- `The egress filter is on.`, the first line of `pco egress show`, and the line
  `Targets:` and its indented entries;
- `was loaded again` in the egress event of a reloaded table;
- `the MAC of <address> moved` in the egress event of a moved MAC;
- `adoption of <name> waits` and `connector is not ready` in the admin event of an
  adoption that waits;
- `the Cloudflare API is overridden to <url> (PCO_CLOUDFLARE_API_URL); this is for tests
  only`, the problem line of the override;
- `service address <service> allocated` and `service address <service> of <address> on
  network <id> released` in the network events;
- `was replaced by the guest's own card` in the managed event of a clone's copied card,
  `automatic attaches resumed` in the managed event of the registered tags back,
  `cf-tunnel-managed=off` in the reason of a grace the marker started, and `attached`,
  `pending-reboot`, `awaiting-dhcp`, `detaching`, `link-down` and `removed` as the states
  and actions of the managed network show them (`engine.Attachment*` and
  `engine.Foreign*`);
- `The namespace pco is as pco left it.` and exit 0 of `pco netns status`;
- `answer from <leg>:67 yiaddr=<lease> type=5 xid=same`, what the suite's own renewal
  prints in a machine.

In the appliance profile also:

- `Profile:` and `Identity: ok (lxc/<vmid> on <node>)` in `pco status`, and
  `Rejected since the table was loaded: 0 packets` in `pco net show`;
- `segment <bridge> is not acknowledged; pco segment acknowledge <bridge>` in the
  reason of a route, `pinned MAC <mac> for <hostname>` in the claim event, and
  `MAC changed from <old> to <new>` and `address <ip> is the gateway of the appliance`
  among the reasons a guest waits for;
- `pco serves nothing while it does`, ending in the `pveum acl modify ... --roles
  NoAccess` lines, and `<token> is not privilege-separated: it holds the roles of
  <user>`, in the problem lines of an exposed appliance;
- `a NIC of the appliance lxc/<vmid> appeared or vanished` in the problem line of a
  card that came or went;
- `<guest> is configured with a MAC of the appliance lxc/<vmid>` in the identity event
  of a tenant, and `configured with the MAC <mac> of the appliance lxc/<vmid>` in the
  reason of its route;
- `the volume at /var/lib/pco is` in the reason of a copy, and `run pco appliance
  recover` in the hold of an appliance behind Cloudflare;
- `epoch drawn after a container start; the state is the volume's` in the writer event,
  and `after a container start` in doctor's `epoch`;
- `pco appliance repair --vmid <vmid>` in the journal and in `pco status` of a restore,
  and `lxc/<vmid> is a copy of lxc/<original>, which is still there` in the repair's
  refusal;
- `Before an upgrade, take a snapshot of this appliance on the node` in `pco upgrade`;
- `nftables: 1`, `the datacenter firewall is off` and `pve-firewall is` in the refusal
  of the managed network, and `needs-firewall-confirmation` in the reason a guest waits.

In the profiles of a cluster also:

- `this node is not the controller; run it on <node>` in the refusal of a node that is
  not the controller, and `is active` in the node event of a node that joined;
- `node <node> has no pco agent with a running watch; routes of other nodes' guests are
  pinned to their nodes`, the problem line of lost coverage;
- `waiting for the agents of <node> to report to this controller`, in the problem line
  or the hold of a takeover;
- `the cluster is not quorate; nothing is written`, the problem line of a node without
  quorum;
- `Node <node> is forgotten.` in `pco node forget`, `pvecm delnode <node>` in its refusal
  of a node still in the cluster, and `removed` in the reason an agent stood down;
- `other-source` in the agent endpoint's refusal of a certificate from another node's
  address;
- the check names `answer on <node>`, `agent channel`, `agent dial`, `coverage` and the
  other checks of the cluster in `pco doctor --json`.
