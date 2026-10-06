# Decide who may publish

This guide goes through what decides who can make pco publish a hostname, in Proxmox VE and in
pco, and tightens it where the people who edit guests are not the people who run the node:
the gate tag, the hostname policy, admission mode `approve` with its approvals, and the
permissions a pool hands out.

## When you need it

- Users other than the admins of the node may edit guests, their Notes or their network, or
  clone them.
- `pco doctor` warns in its `admission` check.
- A clone or a restored copy of a guest should not publish anything until someone has looked.

## Who can do what

Each of these lets someone take part in what pco publishes:

| Who, in Proxmox VE | Can | Held by |
|---|---|---|
| `Sys.Modify` on `/` | Set or take off the gate tag, when it is registered; be an admin of pco in the web interface. | The admins. |
| `VM.Config.Options` on a guest | Write its Notes, and so choose the hostnames it asks for. | The owners of the guest. |
| `VM.Clone` on a tagged guest, and `VM.Allocate` where the clone goes | Make a tagged guest of their own, whose Notes they may then fill. | Often more people than you think. |
| `VM.Config.Network` on a guest | Change the MAC and the static address of its cards. | The owners of the guest. |
| Root on the node | Everything pco does: its commands, its settings, its manual routes. | The admins. |

None of these alone is enough: the gate tag says a guest may publish, its Notes say what it asks
for, the settings say which names may be published at all, and an address is served only once
pco has proven on the network that it is the guest's ([Identity](../identity.md)).

A role granted on a pool, `/pool/<name>`, counts for every guest in the pool. pco reads the
pool of each guest, which is what `Pool.Audit` in its role `PCO` is for, and counts a role on
the pool as one on the guest when it decides whether anyone but the admins may change the
network of a guest. To see what one user may do on one guest, through the guest, a pool or a
group:

```sh
pveum user permissions alice@pve --path /vms/120
```

`pveum acl list` lists every grant of the cluster.

## Before you start

- You are root on the node.
- Know which guests carry the gate tag: `pco guest list` lists them under `Tagged guests`.

## Steps

1. Check that the gate tag is a registered tag, which only `Sys.Modify` on `/` may set:

   ```sh
   pvesh get /cluster/options --output-format json | jq -r '."registered-tags"'
   ```

   It should name `cf-tunnel`, `cf-tunnel-managed` and the gate tag of the settings when that
   is another one. If it does not, because setup ran with `--no-registered-tags` or `gateTag`
   was changed later:

   ```sh
   pco setup --repair
   ```

2. Limit what the Notes may name. List the names that may be published in `allowHosts` and
   the ones that never may in `denyHosts`; a deny pattern wins:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq '.settings.allowHosts = ["app.example.com", "www.example.com", "api.example.com"]
       | .settings.denyHosts = ["*.internal.example.com"]' \
     /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

   A wildcard pattern such as `*.example.com` admits every name below the zone and every
   wildcard there too, so it is no limit for Notes you do not trust. `maxHostnamesPerGuest`,
   32 by default, caps the names one guest may ask for. A name the policy refuses is an issue
   of the guest, `hostname "x" is not allowed by policy`, and is not published.

3. Approve the guests that publish now, before you switch the mode: once it is `approve`, every
   guest without an approval stops publishing at the next cycle. In admission mode `tag` an
   approval is recorded and waits:

   ```sh
   pco guest approve lxc/120
   ```

   ```text
   Approved lxc/120 (app-1) in identity mac:3f2a91b47c0d5e6f8a9b0c1d2e3f4a5b.
   The admission mode is tag: the approval matters only for its routes at observed until the mode is approve.
   ```

   An approval is of the identity the guest has now: for a VM its SMBIOS UUID, for a container
   a hash of the MAC of its first card. A clone has another one.

4. Switch the admission mode:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq '.settings.admission = "approve"' /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

5. From now on a tagged guest without an approval waits. `pco status` counts them:

   ```text
   Approval:    1 guest waits (pco guest list)
   ```

   and `pco guest list` names them, with why:

   ```text
   Approved guests:
     GUEST            IDENTITY                              NOW
     lxc/120 (app-1)  mac:3f2a91b47c0d5e6f8a9b0c1d2e3f4a5b  the same

   Waiting for approval (pco guest approve <owner>):
     lxc/121 (app-1-clone): admission mode approve

   Tagged guests:
     GUEST                  NODE   RUNNING  IDENTITY                              APPROVAL  ROUTES  ISSUES
     lxc/120 (app-1)        node1  yes      mac:3f2a91b47c0d5e6f8a9b0c1d2e3f4a5b  approved  1       0
     lxc/121 (app-1-clone)  node1  yes      mac:9d41c07e2b3a4c5d6e7f8091a2b3c4d5  waiting   0       0
   ```

6. Look at what a waiting guest would publish, and approve it if you agree. The command shows
   it before it records anything:

   ```sh
   pco guest approve lxc/121
   ```

   ```text
   lxc/121 (app-1-clone) waits for approval in identity mac:9d41c07e2b3a4c5d6e7f8091a2b3c4d5; approved, it publishes app-test.example.com.
   It waits because:
     admission mode approve
   Approved lxc/121 (app-1-clone) in identity mac:9d41c07e2b3a4c5d6e7f8091a2b3c4d5.
   From the next cycle its routes no longer wait for an approval.
   ```

   The daemon refuses the approval when the guest changed since it was shown; run it again.

## Check

```sh
pco doctor
pco guest list
```

The `admission` check of `pco doctor` says `approve: a tagged guest is published once an admin
approved it`, and a check `approval <owner>` warns for each guest that waits. In mode `tag`,
with tagged guests, the `admission` check warns that whoever may clone one makes a tagged guest
of their own.

A guest that waits publishes nothing. A hostname it held already stays claimed and answers 503,
so that nobody else takes it meanwhile.

## Undo

Take an approval back, which stops the guest publishing while the mode is `approve`, and holds
its hostnames for it:

```sh
pco guest revoke lxc/121
```

Set `admission` back to `tag` the way it was set. The approvals stay in the store and count
again if you switch back; they also release the routes of a guest that wait at the `observed`
level, in either mode ([Run pco on a cluster](clusters.md)).

## Read on

- [Security](../security.md#the-gate-tag-and-what-it-proves): what the tag proves, clones and
  restores, and approval mode in full.
- [Settings](../settings.md#publishing): `admission`, `allowHosts`, `denyHosts`,
  `maxHostnamesPerGuest`.
- Reference: [pco guest approve](../cli/pco-guest-approve.md),
  [pco guest list](../cli/pco-guest-list.md), [pco guest revoke](../cli/pco-guest-revoke.md).
