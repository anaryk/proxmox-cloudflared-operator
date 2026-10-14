# Upgrade and go back

This guide upgrades pco and `cloudflared` on a node, and on every node of a cluster, keeps
`cloudflared` to the versions you choose, and says what going back to an earlier version means.
pco and `cloudflared` are two packages and are upgraded apart.

## When you need it

- A new release of pco is out.
- A new `cloudflared` is out, or `pco doctor` warns that the one on the node is more than ten
  months old.
- An upgrade misbehaves, and you want to know how to go back.

## What an upgrade touches

pco is installed from a package file, not from an apt repository, so `apt upgrade` never
upgrades it. An upgrade of the package reloads systemd and restarts `pco.service` and
`pco-web.service` if they ran. The connectors are not touched: published hostnames keep
answering while the daemon restarts, and only the decisions, new routes and withdrawals, wait
until its first cycle.

`cloudflared` comes from Cloudflare's apt repository, which `pco setup` adds when it installs
it, and `apt upgrade` upgrades it with the rest of the node. A connector runs on with the binary
it started with until it restarts.

## Before you start

- Read the release notes of the new version for a change you have to act on first. A release
  can, for instance, publish a kind of hostname only once a setting names it; the notes say
  which setting to add before the upgrade.
- Save what an admin decided, as [Back up and recover](backup-and-recovery.md#back-up) says, and
  have a backup of the node itself from before the upgrade if you may want the old version back.

## Upgrade pco

1. Install the new package with the installer, which checks the signature and the checksum
   as on the first install and then runs `pco setup`. `PCO_VERSION` names the release, and
   `PCO_PROFILE=host` answers beforehand the question whether to install on the node or as an
   appliance:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | PCO_PROFILE=host PCO_VERSION=1.3.0 bash
   ```

   Or download the files and let the installer check them, as
   [Quickstart](../quickstart.md#install) describes, and run `pco setup` yourself afterwards.

2. `pco setup` changes nothing that is in order and writes the new version into the node's
   entry of the node registry:

   ```text
   node registry: refreshed node1
   ```

3. Check the node:

   ```sh
   pco version
   pco status
   pco doctor
   pco egress show
   ```

   When a release changes the egress filter, the table in place is not the one the new daemon
   loads, and `pco egress show` and `pco status` say so until the daemon's next check loads
   it, within 30 seconds; `pco egress load` does it at once.

`pco upgrade` is the appliance's command and is refused on a node: `pco upgrade runs inside the
appliance; a host upgrades with apt: the installer installs a newer pco package, and apt-get
install --only-upgrade cloudflared a newer cloudflared`.

## Upgrade cloudflared

Each release of pco lists the versions of `cloudflared` it was tried with, and those it found
wanting, in `/usr/share/pco/cloudflared-versions.json`:

```sh
jq -r '.versions[].version, (.deny[] | "denied: \(.version) (\(.reason))")' /usr/share/pco/cloudflared-versions.json
```

```text
2026.9.3
denied: 2026.9.0 (cloudflare/cloudflared#1737)
```

On a node nothing enforces the list; it tells you what to choose.

1. To upgrade only when you choose, hold the package, so that `apt upgrade` leaves it:

   ```sh
   apt-mark hold cloudflared
   ```

   A held package gets no update at all, security fixes included, so look at it at every
   release of pco; `pco doctor` warns when it is more than ten months old.

2. Upgrade it:

   ```sh
   apt-mark unhold cloudflared
   apt-get install --only-upgrade cloudflared
   apt-mark hold cloudflared
   ```

   To install a version of the list rather than the newest, take its package from the list and
   check it before you install it:

   ```sh
   jq -r '.versions[] | select(.version == "2026.9.3") | .amd64.url, .amd64.sha256' /usr/share/pco/cloudflared-versions.json
   curl -fsSLO https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb
   echo 'bc073ef293d504cf5ac533bd0aa1c824ef6b4f358765ccaa6628a8a95cacb4b7  cloudflared-linux-amd64.deb' | sha256sum --check
   apt-mark unhold cloudflared
   apt install ./cloudflared-linux-amd64.deb
   apt-mark hold cloudflared
   ```

3. Restart the connectors on the new binary, one at a time, and let each be ready before the
   next. A connector that stops gives its open requests up to 30 seconds, and the hostnames of
   its tunnel are unreachable for the moment it takes to connect again:

   ```sh
   systemctl list-units 'pco-cloudflared@*'
   systemctl restart pco-cloudflared@0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
   pco status
   ```

   Its line under `Tunnels` says `active, ready` again when it is.

## On a cluster

Every node of a cluster runs the package, and you upgrade them one at a time. A build speaks the
protocol of its release and of the one before, so the controller and the agents may run either
while the others are upgraded, and `pco cluster status` and `pco doctor` show which node runs
what.

1. **The nodes that are not the controller first.** On each, install the new package as above,
   then check that it is back before the next:

   ```sh
   pco status
   ```

   Its `Agent:` line says `hears the controller`, and on the controller `pco node list` shows the
   node `active` again: an upgrade restarts the daemon, and the node passes through `joining`
   once its agent reports. The connectors run on throughout.

2. **The controller last.** Hand the lease to an upgraded node first, so that the hand-over takes
   seconds, then upgrade the node that held it:

   ```sh
   pco cluster handoff --to node2
   ```

   A restart of the package on the node that holds the lease gives it back by itself, and the
   node that takes it next draws a new epoch, which rewrites the sentinel of every tunnel once.
   A node that stands alone keeps its epoch over the restart.

3. **Finish the upgrade**, from an earlier release that ran on one node, once every node runs
   the new one. Until then the earlier release can still be installed again, and the settings
   this release adds cannot be set:

   ```sh
   pco cluster status
   ```

   ```text
   Upgrade:     not finished since 2026-10-01T14:00:00+02:00, from 0.9.0 (pco cluster finish-upgrade)
   ```

   Write a bundle to keep, then finish on the controller:

   ```sh
   pco export /root/pco-before-finish.bundle
   pco cluster finish-upgrade
   ```

   It is refused while a node that was not forgotten runs an older build, a node that is down
   included: forget a dead node first. It copies the store below `/var/lib/pco/backup` on the
   controller's node before it changes anything.

**cloudflared** on a cluster is upgraded on each node by apt, and its connectors restarted on each
node in turn: restart them on one node, wait until `pco cluster status` shows that node's replica
`ready` again, then go on to the next, so that the shared tunnel always has a ready replica.
In the appliance, `pco upgrade` in an agent container restarts its connectors only while the
controller counts another ready replica of the tunnel, and waits for one up to five minutes.

To go back before the upgrade is finished, see
[the runbook](cluster-runbooks.md#an-upgrade-is-not-finished): the earlier release ran on one node.

## Go back to an earlier version

**cloudflared.** Install the earlier version the same way as a chosen one above, never one the
list denies, hold it, and restart the connectors one at a time.

**pco.** pco does not support a downgrade, and this guide gives no steps for one, but for the
release before this one while an upgrade from it is not finished ([on a cluster](#on-a-cluster)). An earlier
build is not tested against what a later one wrote, and the host has no rollback of its own (the
appliance has one, which [Upgrade the appliance](appliance-upgrade.md) describes). What an
earlier build does with the store is fixed, though: it refuses a file of a newer schema version
and never rewrites it, and it refuses a settings file with a setting it does not know. Its
daemon then holds, with `reading the settings: ...` among the problems, and changes nothing; the
connectors keep serving what was published.

If an upgrade leaves pco in a state you cannot work with, the way back is a backup of the node
from before it: a restored node has the earlier package and its store as they were.
[After a restore of the node](backup-and-recovery.md#after-a-restore-of-the-node) says what to do
next. The archive of [Back up and recover](backup-and-recovery.md#back-up) holds what an admin
decided and the secrets, not the package.

## Check

```sh
pco version
pco status
pco doctor
```

`pco status` has no problem, its tunnels are `VERIFIED yes` and their connectors `active,
ready`; the `cloudflared` check of `pco doctor` names the version that runs.

## Undo

An upgrade of pco is not undone by a downgrade; see above. A hold is lifted with
`apt-mark unhold cloudflared`, after which `apt upgrade` keeps `cloudflared` current again.

## Read on

- [Operations](../operations.md#upgrades): upgrades in the reference.
- [Files](../files.md#what-the-package-installs): what the package installs.
- Reference: [pco setup](../cli/pco-setup.md), [pco version](../cli/pco-version.md),
  [pco upgrade](../cli/pco-upgrade.md), [pco cluster handoff](../cli/pco-cluster-handoff.md),
  [pco cluster finish-upgrade](../cli/pco-cluster-finish-upgrade.md).
