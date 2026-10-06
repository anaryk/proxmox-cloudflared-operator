# Restore the appliance from a backup

## When you need it

The appliance is lost, with its node or its disk, and a backup of it is at hand; or its
container is damaged and you restore the backup over it. A backup of the appliance never holds
its state: the state volume is kept out of backups, as it holds the secrets. So a restore brings
the container back, and a repair brings back its install, from what Cloudflare and Proxmox know.

## Before you start

- A backup of the appliance, made by `vzdump`, best in snapshot mode.
- A Cloudflare token that sees the account of the appliance's tunnel, in a file only root can
  read on the node. The restored appliance finds its install through it.
- The release of pco the appliance ran, for the repair on the node; see
  [commands on the node](../appliance.md#commands-on-the-node).
- The original appliance gone from the cluster. A restore beside it is a copy, which serves
  nothing and which the repair refuses.

## Steps

1. Restore the backup into pool `pco`, to a new VMID or over the appliance. Over it, take its
   protection off first, as Proxmox refuses to restore over a protected container:

       pct restore 121 /var/lib/vz/dump/vzdump-lxc-120-2026_10_06-02_00_01.tar.zst --storage local-zfs --pool pco

       pct set 120 --protection 0
       pct restore 120 /var/lib/vz/dump/vzdump-lxc-120-2026_10_06-02_00_01.tar.zst --storage local-zfs --pool pco --force 1

   The restored container has a new, empty state volume. Its daemon refuses to start without the
   marker of a volume of pco, and stays failed, which is expected. The steps below use 121; over
   the appliance, it is 120.

2. Repair it with the token, on the node, with the binary of the release unpacked as
   [commands on the node](../appliance.md#commands-on-the-node) says:

       /root/pco-1.2.3/usr/bin/pco appliance repair --vmid 121 --recover --cf-token-file /root/cf-token

   It starts the container, marks a container restored to a new VMID as itself, marks the new
   volume, makes the Proxmox token anew, and has the appliance adopt the install whose tunnel the
   token sees. With several installs in reach it lists them, and `--install-id` chooses. It stops
   the container again at the end if it was stopped.

3. Protect the container again, and start it if it is stopped:

       pct set 121 --protection 1
       pct start 121

4. The install is back in observe-only mode, with the default settings of an appliance. Put
   back what an admin decided, which no backup held: the settings you had, the acknowledged
   segments, the approvals and the manual routes. Then:

       pct exec 121 -- pco plan
       pct exec 121 -- pco apply

## Check

    pct exec 121 -- pco status
    pct exec 121 -- pco doctor

`pco status` shows `Identity: ok (lxc/121 on pve1)` and the tunnel of the install, verified once
applied. `pco doctor` fails its `protection` check while the container is not protected, and its
`volume` check while `mp0` is not kept out of backups.

## Undo

A repair takes nothing back, and there is nothing to undo: running it again finishes it. To
remove the restored appliance instead, uninstall it as any appliance, with
`--keep-cloudflare` when the tunnel at Cloudflare is to stay for another one; see
[Uninstall](../uninstall.md#the-appliance).
