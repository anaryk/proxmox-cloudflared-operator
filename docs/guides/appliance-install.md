# Install the appliance

## When you need it

You want pco on a Proxmox VE node without installing anything on the node itself: pco and its
connectors in a container of their own. [Appliance](../appliance.md) says what that gives and
what it costs against the host profile.

## Before you start

- Proxmox VE 8.4 or later, or 9.x, on amd64 or arm64, and root on the node.
- A storage that holds containers, ideally one that can snapshot (ZFS, LVM-thin, Ceph), so
  that backups of the appliance can run in snapshot mode.
- The bridge, and on a VLAN-aware bridge the VLAN, that the guests you will publish are on.
  The appliance reaches guests on its own segment, and reaches the Proxmox API at the node's
  address on that segment unless you name another with `--api-host`.
- An address for the appliance: a static one, or DHCP, ideally with a reservation. Its web
  interface follows the address of its card, so a reservation keeps it where you expect it.
- Outbound access from that segment to Cloudflare's API on port 443 and its edge on port 7844,
  TCP and UDP, and to Debian's mirrors for the security updates.
- A Cloudflare token as [Cloudflare token](../cloudflare-token.md) describes, in a file only
  root can read, or added later.
- No principal but the admins with privileges on `/vms` or `/`. The installer names any it
  finds; see [who may reach the appliance](../appliance.md#who-may-reach-the-appliance).

## Steps

1. Run the installer on the node with the appliance profile, the storage and, if you have it,
   the token:

       curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash -s -- --appliance --storage local-zfs --ip 192.0.2.120/24,gw=192.0.2.1 --vmid 120 --cf-token-file /root/cf-token

   To check the release key the script carries before it runs, take the script from the tag of
   the release as
   [SECURITY.md](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/SECURITY.md#with-the-installer)
   shows, and run that copy with the same arguments.

   On a VLAN-aware bridge add `--vlan`, as `--vlan 20`. The installer prints what it found
   first, then what it makes, and ends with the next steps and the fingerprint of the web
   interface's certificate. Keep the fingerprint.

2. Answer its questions. It asks whether to register the gate tags (yes), and only when it
   finds them, whether to go on beside another install of pco and whether to add `NoAccess`
   lines for principals that could reach into the appliance. Read what each line takes away
   before you say yes.

3. If you gave no token, add one:

       pct exec 120 -- pco credential add --label main

4. Tag a guest on the appliance's segment with `cf-tunnel` and write its routes into its Notes,
   as in [Quickstart](../quickstart.md#tag-a-guest-and-write-its-routes).

5. Acknowledge the segment once its routes show up as waiting for it:

       pct exec 120 -- pco routes
       pct exec 120 -- pco segment acknowledge vmbr0

   Use `vmbr0:20` for VLAN 20 of `vmbr0`.

6. Approve the guests that wait, after reading why:

       pct exec 120 -- pco guest list
       pct exec 120 -- pco guest approve qemu/132

7. Look at the plan, and publish:

       pct exec 120 -- pco plan
       pct exec 120 -- pco apply

## Check

    pct exec 120 -- pco status
    pct exec 120 -- pco doctor

`pco status` shows `Profile: appliance` and `Identity: ok (lxc/120 on pve1)`, and once applied
the tunnel with its connector ready. `pco doctor` runs the checks of the appliance besides the
usual ones; every line should be `✓`. Open `https://192.0.2.120:8643/`, compare the fingerprint
the browser shows with the one the installer printed, and sign in with a Proxmox VE user that
holds `Sys.Modify` on `/`.

## Undo

On the node, with the release the appliance runs:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | PCO_VERSION=1.2.3 bash -s -- --appliance --uninstall --vmid 120

It runs `pco appliance uninstall`, which lists what it removes and asks, and asks again what
becomes of the tunnel and the records at Cloudflare; [Uninstall](../uninstall.md#the-appliance)
has the details. A run of the installer
that failed has taken back what it made already; one that was killed is finished or taken back
with `PCO_RESUME` (see [the appliance](../appliance.md#a-run-that-was-cut-short)).
