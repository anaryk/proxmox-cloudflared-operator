# End-to-end suite

The suite runs pco as it is installed on a Proxmox VE node: the package, `pco setup`,
the daemon under systemd, real containers, the egress filter in nftables, and
`pco uninstall` at the end. Cloudflare is a fake served by the suite itself on a
loopback port, or the real API with a zone of yours.

It changes the node it runs on, so run it on a test node only.

## What it needs and what it touches

- Proxmox VE 8.4 or later, run as root. Without root, `pct` or pco it skips.
- pco installed from a package, and not set up: no `/etc/pve/pco`.
- The bridge `vmbr1`, without the address `10.77.0.1/24`. The suite gives it that
  address for the run, and brings it up for the run when it is down; the containers
  get `10.77.0.<vmid - 9000>/24` on it.
- No container with an id from 9100 to 9199. The suite makes its containers there,
  named `e2e-<vmid>`.
- The Alpine template `local:vztmpl/alpine-3.24-default_20260714_amd64.tar.xz` and
  the storage `local-lvm`; `PCO_E2E_TEMPLATE` and `PCO_E2E_STORAGE` name others.
- `/usr/bin/cloudflared` against the real API. With the fake the suite does without:
  setup then runs with `--skip-cloudflared` rather than install it, and the
  connectors' units are enabled but cannot start, which with the fake's token they
  could not do anyway.

With the fake, the suite writes the drop-in `/etc/systemd/system/pco.service.d/e2e.conf`,
which points the daemon at it with `PCO_CLOUDFLARE_API_URL`, and passes the same variable
to `pco setup` and `pco uninstall`. That variable is for tests only: pco accepts it for an
`http` or `https` URL of a loopback address or `localhost`, warns when it starts, and
`pco status` shows it as a problem for as long as it is set.

At the end the suite uninstalls pco with `--purge-cloudflare`, destroys its containers,
removes the drop-in and the address and takes the bridge down again if it was down,
also when a scenario failed. `cleanup.sh` does the same by hand, for a run that was cut
off, and purges the package and removes the user `pco-connector` as well.

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

Against the real Cloudflare API, give a token as described in
[Cloudflare token](../../docs/cloudflare-token.md) and a zone it may change:

    PCO_E2E_CF_TOKEN=... PCO_E2E_ZONE=example.com ./e2e.test -test.v -test.timeout 90m

The suite then publishes names below `e2e-*.<zone>`, fetches one of them through
Cloudflare, and deletes everything again with the purge. The scenarios that need the fake
(a token that loses a permission, an outage of the API) are skipped.

## Scenarios

The ids are those of the test matrix.

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
| S13 | The daemon killed with SIGKILL, `pveproxy` stopped, the API down: the daemon holds and changes nothing, and everything is as before afterwards. |
| S14 | `nft flush ruleset`: the egress table comes back with its targets, with an event. Tables of others that do not come back by themselves are loaded again as they were. |
| S15 | A published container takes another MAC: its target leaves the egress set within 2 seconds, and comes back with the MAC. |
| S9 | A container that answers for the node's address and for another guest's, and names both: never published to either. |

The grace is 30 seconds and the poll interval 5 seconds for the run. A full run takes
about half an hour.
