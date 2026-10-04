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
- No guest, container or VM, with an id from 9100 to 9199, and no guest tagged
  `cf-tunnel`: the daemon would serve it too, and against the real API publish it.
- The Alpine template `local:vztmpl/alpine-3.24-default_20260714_amd64.tar.xz` and
  the storage `local-lvm`; `PCO_E2E_TEMPLATE` and `PCO_E2E_STORAGE` name others.
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
- it destroys the containers 9100 to 9199 named `e2e-*`, removes the drop-in, the
  address and the token file, and takes the bridge down when the suite brought it up;
- then it purges the package and removes the user `pco-connector`, and says what
  should be gone and is not.

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

A target counts as absent from the egress set only while `pco egress show` exits 0 and
says the filter is on: a table that is not loaded has no target either.

The grace is 30 seconds and the poll interval 5 seconds for the run. A full run takes
about half an hour.

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
  only`, the problem line of the override.
