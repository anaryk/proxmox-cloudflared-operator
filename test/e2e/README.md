# End-to-end suite

The suite runs pco as it is installed on a Proxmox VE node: the package, `pco setup`,
the daemon under systemd, real containers, the egress filter in nftables, and
`pco uninstall` at the end. Cloudflare is a fake served by the suite itself on a
loopback port, or the real API with a zone of yours.

It changes the node it runs on, so run it on a test node only.

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
  them at the end; makes guests 9350 to 9399 on them, containers and, with a cloud
  image, virtual machines; attaches networks to them and detaches them again, which
  enables `pco-netns.service` and brings up the gateway namespace `pco`; enables the
  managed network (`pco network managed enable`: the SDN zone `pco` and vnet
  `pconet`, applied with an SDN reload) and lets the daemon put cards on its guests,
  with `managed.detachGrace` at one minute for the run; adds `cf-tunnel-managed` to
  the registered tags and takes them away for a while, putting them back as they
  were before pco is uninstalled; sets `net.bridge.bridge-nf-call-iptables=1` and
  `net.ipv4.ip_forward=1` and puts them back; starts `pve-firewall` when it is not
  running and stops it again; restarts `pco-netns` and `pco`, and reinstalls the
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
- it puts the kernel parameters back, stops `pve-firewall` when the suite started it,
  and removes the runtime bridges the suite made;
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

The appliance profile has no end-to-end suite yet: the variants of S17, S19, S21 and
S22 for an appliance (the `filtered` level, the refusals while the node's firewall
backend, the datacenter firewall or `pve-firewall` do not serve it, the firewall-impact
confirmation, pco's rules in the guest's chain) are run by hand in the lab until it has
one.

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
