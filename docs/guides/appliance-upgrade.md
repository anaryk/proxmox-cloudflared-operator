# Upgrade the appliance

## When you need it

A new release of pco is out, or a newer `cloudflared` that the list of vetted versions allows,
or `pco doctor` warns in its `versions` check. Debian's security updates need none of this: the
appliance installs them every day by itself.

## Before you start

- The appliance runs and `pco doctor` has no failure that is not about versions.
- Room on the node's storage for a snapshot of the appliance, its root filesystem and its state
  volume.
- A moment when the connectors may restart one after the other: each has 60 seconds to be ready
  again, and while a tunnel's only connector restarts, its hostnames do not answer.

## Steps

1. See what is installed and what is available:

       pct exec 120 -- pco upgrade --check

       release v1.2.4: checksums.txt is signed by key 0123456789ABCDEF0123456789ABCDEF01234567
       pco: installed 1.2.3, available 1.2.4
       cloudflared: installed 2026.9.3, available 2026.10.1, from the manifest of release v1.2.4 of 2026-10-05
       cloudflared denied: 2026.9.0 (cloudflare/cloudflared#1737)

   It exits 1 when an upgrade is available and 0 when there is none.

2. On the node, take a snapshot of the appliance, with today's date in the name:

       pct snapshot 120 pco-pre-upgrade-20261006

   pco cannot take it itself: its token has no right to. `pco doctor` accepts a snapshot of this
   name for 7 days and warns about it after, as every snapshot carries the secrets.

3. Upgrade, pco first and then `cloudflared`:

       pct exec 120 -- pco upgrade

   It reminds you of the snapshot, says what it will change, and asks. Without a terminal, `--yes`
   answers. A run looks like this:

       release v1.2.3: checksums.txt is signed by key 0123456789ABCDEF0123456789ABCDEF01234567
       release v1.2.4: checksums.txt is signed by key 0123456789ABCDEF0123456789ABCDEF01234567
       Before an upgrade, take a snapshot of this appliance on the node, as root there:
         pct snapshot 120 pco-pre-upgrade-20261006
       pco cannot take it: its token has no right to. A rollback to that snapshot is followed by
       pco appliance recover in the appliance.
       The connectors restart on the new cloudflared: a tunnel has no connection from this appliance
       until its connector is ready again, at most 1m0s.
       Upgrade pco from 1.2.3 to 1.2.4 and cloudflared from 2026.9.3 to 2026.10.1? [y/N] y
       From here a signal stops pco upgrade only once what it installs is held again.
       installing pco_1.2.4_amd64.deb
       pco 1.2.4 is installed, from release v1.2.4 signed by key 0123456789ABCDEF0123456789ABCDEF01234567
       pco 1.2.3 is kept as /var/lib/pco/upgrades/previous/pco_1.2.3_amd64.deb for --rollback
       the daemon runs pco 1.2.4
       installing cloudflared_2026.10.1_amd64.deb
       cloudflared 2026.10.1 is installed
       cloudflared 2026.9.3 is kept as /var/lib/pco/upgrades/previous/cloudflared_2026.9.3_amd64.deb for --rollback
       pco and cloudflared are held

   `pco upgrade pco` or `pco upgrade cloudflared` upgrades one of them, and `--version` names the
   version to install, as `pco upgrade cloudflared --version 2026.10.1`.

## Check

    pct exec 120 -- pco version
    pct exec 120 -- pco status
    pct exec 120 -- pco doctor

The connectors are `active, ready` again in `pco status`, and `pco doctor` says in `versions`
what runs and in `holds` that pco and `cloudflared` are held.

## Undo

Go back to the package the upgrade replaced, inside the appliance:

    pct exec 120 -- pco upgrade --rollback

`pco upgrade cloudflared --rollback` takes back `cloudflared` alone. A rollback of pco is refused
while the store holds an object the older pco cannot read, and one of `cloudflared` to a version
the list denies.

When that is not enough, roll the container back to the snapshot on the node, and start it:

    pct rollback 120 pco-pre-upgrade-20261006 --start 1

What follows depends on whether the container started between the snapshot and the rollback,
as a reboot of the node does:

- It did not: the daemon goes on with the state of the snapshot. `pco events` has the event
  `epoch drawn after a container start; the state is the volume's`.
- It did: the daemon holds every write, its `Writer:` line says `behind`, and its problem says
  that the state of the appliance is older than its last write at Cloudflare. Then:

      pct exec 120 -- pco appliance recover
      pct exec 120 -- pco plan
      pct exec 120 -- pco apply

Either way, approvals, acknowledged segments and credentials made since the snapshot are gone
with it; make them again. Remove the snapshot once you no longer need it:
`pct delsnapshot 120 pco-pre-upgrade-20261006`.
[Appliance](../appliance.md#snapshots-backups-and-their-limits) explains why.
