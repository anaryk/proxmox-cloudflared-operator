# Upgrade and go back

This guide upgrades pco and `cloudflared` on a node, keeps `cloudflared` to the versions you
choose, and says what going back to an earlier version does. pco and `cloudflared` are two
packages and are upgraded apart.

## When you need it

- A new release of pco is out.
- A new `cloudflared` is out, or `pco doctor` warns that the one on the node is more than ten
  months old.
- An upgrade misbehaves, and you want the version before it back.

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
- Save what an admin decided, as [Back up and recover](backup-and-recovery.md#back-up) says.

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

2. `pco setup` changes nothing that is in order and notes the new version in the node
   registry:

   ```text
   node registry: node1 now runs version 1.3.0
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

## Go back to an earlier version

**cloudflared.** Install the earlier version the same way as a chosen one above, never one the
list denies, hold it, and restart the connectors one at a time.

**pco.** pco does not support a downgrade: an earlier build is not tested against what a later
one wrote, and the host has no rollback of its own (the appliance has `pco upgrade --rollback`).
What an earlier build does with the store is fixed, though: it refuses a file of a newer schema
version and never rewrites it, and it refuses a settings file with a setting it does not know.
Its daemon then holds, with `reading the settings: ...` among the problems, and changes
nothing; the connectors keep serving what was published.

If you go back all the same:

1. Compare the schema version of the store each build reads. If they differ, do not go back:

   ```sh
   pco version --json
   ```

   ```text
   {
     "version": "1.3.0",
     "commit": "3e4f5a6",
     "date": "2026-10-01T12:00:00Z",
     "schemaVersion": 1
   }
   ```

2. Install the earlier package over the later one, after checking it as for an install:

   ```sh
   apt install --allow-downgrades ./pco_1.2.3_amd64.deb
   pco setup
   ```

3. If the later version added a setting that the settings file now has, the earlier daemon
   holds with `reading the settings: <file>: invalid: json: unknown field "<name>"`. Remove the
   field from `/etc/pve/pco/meta/settings.json`, from the object in `data`, with an editor;
   the settings file is the one state file you may edit by hand
   ([Settings](../settings.md#compatibility)).

## Check

```sh
pco version
pco status
pco doctor
```

`pco status` has no problem, its tunnels are `VERIFIED yes` and their connectors `active,
ready`; the `cloudflared` check of `pco doctor` names the version that runs.

## Undo

An upgrade of pco is undone by going back, above. A hold is lifted with
`apt-mark unhold cloudflared`, after which `apt upgrade` keeps `cloudflared` current again.

## Read on

- [Operations](../operations.md#upgrades): upgrades in the reference.
- [Files](../files.md#what-the-package-installs): what the package installs.
- Reference: [pco setup](../cli/pco-setup.md), [pco version](../cli/pco-version.md),
  [pco upgrade](../cli/pco-upgrade.md).
