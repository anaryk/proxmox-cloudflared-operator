# Preview environments

This page describes pco for a team that makes a guest for each pull request and wants it
reachable at a name of its own while the pull request is open. The names come and go
automatically, and the person who makes them is a CI job.

## The situation

A team reviews changes in running copies of the application. When a pull request opens, a CI
job clones a template container into a new container, writes a hostname such as
`pr-121.example.net` in its Notes and starts it. When the pull request closes, the job
destroys it. Reviewers open the name in a browser. Nobody should have to run a command of pco
for any of this, and a job that goes wrong should not be able to publish anything that is not
a preview.

The pieces:

- **A node of its own**, with its own pco, for previews and nothing else. pco runs on one node,
  and the guests run code from pull requests.
- **A zone of its own**, `example.net`, on its own token. The names of previews are one level
  below it.
- **A tagged template**, whose clones carry its tag. Proxmox copies the tags of the source to
  a clone, so a CI account that may clone the template gets tagged guests without being able
  to set the tag itself.
- **A CI account**, with rights on a pool and on the template and nowhere else.

## The setup

The job clones the template into the pool `previews`, writes the Notes and starts the guest.
pco reads the tagged guests, proves the address of the new one, and writes the tunnel
configuration and the DNS record. On the close of the pull request the job destroys the
guest and pco takes its name down after the grace period:

~~~mermaid
flowchart TB
    reviewer["Reviewer"] --> edge["Cloudflare edge"]
    edge -->|"through the tunnel"| connector
    subgraph node["Proxmox VE node for previews, host profile"]
        connector["cloudflared connector"] --> filter["Egress filter"]
        filter --> guest["lxc/152: pr-121, tagged, in pool previews"]
        daemon["pco daemon"] -->|"reads the guest, verifies its address"| guest
        template["lxc/150: template, tagged"] -. "its clones carry the tag" .-> guest
    end
    daemon -. "tunnel rule, CNAME pr-121.example.net" .-> edge
    ci["CI job, as the account ci@pve"] -->|"clone, Notes, start; destroy when the pull request closes"| guest
~~~

## Decisions

### Profile

The host profile, on a node of its own. The appliance would prove the previews at `observed`
only, and each would wait for `pco guest approve` while the account that makes the guests
holds `VM.Config.Network` on them, which is the opposite of what a preview is for. A node of
its own keeps untrusted code, and the token and zone of the previews, away from whatever else
runs in the company.

### Admission

Leave `admission` at `tag`. Approval mode asks a person to approve each clone, and a preview
that waits for a person is no preview. The gate is the template instead: only a guest cloned
from the tagged template, by an account that holds `VM.Clone` on it, carries the tag, and the
tag is a registered tag, so only a user with `Sys.Modify` on `/` can set it.

This is the case that [Security](../security.md#clones-and-restores) warns about, and
`pco doctor` will warn about it in its `admission` check for as long as a guest carries the
tag: whoever may clone a tagged guest makes a tagged guest of their own, with Notes they
write. Here that is the design, and what keeps it safe is the rest of this page: the account
can do nothing but make and run clones of the template in one pool, the token reaches one
zone, and a guest may name at most two hostnames.

The Notes of the template name no hostname. Clones copy the Notes, and a clone that names the
hostname of the template would claim it, to be rewritten a moment later and to hold the claim
for the grace period; a second clone in that moment would be in conflict. Until the job writes
the Notes of a clone, `pco status` lists the issue `tagged cf-tunnel but no routes found in
Notes` for it, which is harmless. The template itself is ignored by pco, as templates are.

### Identity minimum

`port`, the default. Every preview runs on this node. For a container nothing more is
needed to learn its address; a virtual machine needs the guest agent in the template, or a
static address.

The previews run in a network of their own, a VLAN of the node's bridge, and the node needs
an address in it to reach them. Before you give it one, restrict the web interface of
Proxmox, `spiceproxy` and `sshd` on that address, as the
[quickstart](../quickstart.md#before-you-start) says: every guest in that network could
otherwise reach them, and these guests run code of pull requests.

### Tokens

One Cloudflare token with the three permissions of
[Cloudflare token](../cloudflare-token.md#permissions), for the account and the zone
`example.net` only. A hostname in any other zone is `no-zone` and takes no claim, so a Notes
block that names `www.example.com` publishes nothing. The production tokens are not on this
node at all.

The CI account has its own Proxmox token. Give it `VM.Clone` on the template, and on the pool
`previews` a role with `VM.Allocate`, `VM.Config.Options` and `VM.PowerMgmt` among what a
clone needs to be made, edited and started, with the storage privileges that making a guest
asks for. It holds nothing on `/`, and in particular not `Sys.Modify`, which would let it set
the tag on any guest and is the privilege of the admins of pco.

### Egress filter

On. The filter confines the connector to the verified targets, whatever the tunnel says. It
does not confine the guests, which run what the pull request brought: what they may reach is
for the Proxmox firewall and the network of the previews.

### Web interface access

The admins of pco are the people who hold `Sys.Modify` on `/`. A developer who wants to see
why a preview is not up can be a reader, with `Sys.Audit` on `/`, who can read the state of the
install and change nothing; on this node that state is previews only. Keep the interface on
the management network.

### Protecting the previews

If the previews are not for the public, put Cloudflare Access in front of them, and read
[Internal tools behind Cloudflare Access](internal-tools-behind-access.md) first: pco does
not manage Access, and a hostname is open until an application covers it. A zone of its own
helps here. Cloudflare lets the hostname of an application be a wildcard, and `*.example.net`
covers every name one level below the zone, `pr-121.example.net` included and the apex not
(Cloudflare's [description](https://developers.cloudflare.com/cloudflare-one/access-controls/policies/app-paths/)),
so one application, made before the first preview, covers all of them.

## What it ends with

The settings:

~~~json
{
  "maxHostnamesPerGuest": 2
}
~~~

The job, written as the commands of the node; it sends the same calls to the Proxmox API
under its token:

    pct clone 150 152 --hostname pr-121 --pool previews
    pct set 152 --description $'```cf-tunnel\npr-121.example.net -> :3000\n```'
    pct start 152

so that the Notes of the new container are

~~~text
```cf-tunnel
pr-121.example.net -> :3000
```
~~~

and, when the pull request closes:

    pct stop 152
    pct destroy 152

The template is tagged once and then made a template, which pco ignores, while its clones
carry the tag:

    pct set 150 --tags 'cf-tunnel'
    pct template 150

## What to watch

- **Tearing down many at once.** Two guards stop a batch. If more than 5 records of the
  install, and more than 30 per cent of them, are being removed, `pco plan` lists them and none
  is deleted until `pco apply --confirm-deletes`. If more than 5 of the guests that hold a
  hostname, and more than 30 per cent of them, drop out of the listing within the grace
  period, the whole cycle holds, which includes new previews, until they are listed again or
  `pco apply --confirm-deletes` says they were removed on purpose
  ([Operations](../operations.md#grace-periods-and-the-mass-delete-guard)). Let the job
  destroy a preview when its pull request closes, and make a nightly sweep remove stale ones
  one at a time, minutes apart. Do not clear a whole pool in one go.
- **How fast a preview comes up.** The daemon looks every 10 seconds. A running guest whose
  address verifies is written to the tunnel in that cycle, and the configuration of a tunnel is
  written at most once in 15 seconds, so the rules of a burst of new previews are written
  together.
- **The same name again.** A second guest for the same pull request, made under another VMID
  while the first is going, waits until the claim of the first ends, a grace period after
  the first is gone. `pco claims list` shows it:

      HOSTNAME            HOLDER            STATE     SINCE                      WAITING                 NOTE
      pr-118.example.net  lxc/151 (pr-118)  held      2026-09-30T12:00:00+02:00  -                       no longer asked for since 2026-10-01T13:59:30+02:00
      pr-121.example.net  lxc/152 (pr-121)  serving   2026-10-01T11:00:00+02:00  -                       -
      pr-124.example.net  lxc/153 (pr-124)  conflict  2026-10-01T13:40:00+02:00  lxc/154 (pr-124-retry)  -

  A guest created under the VMID of one that is gone takes over its claim at once, while the
  claim stands.
- **Leftovers.** A job that dies before it destroys the guest leaves the name up. `pco routes`
  lists every route with its owner, and a sweep of the pool is the answer.
- **Changes to the template.** A hostname written into its Notes makes every clone from then
  on claim it, and all but one are in conflict.
- **The warning.** `pco doctor` shows the `admission` warning on every run. Know it, so that
  it does not hide a new one.
- **The node.** There is one node and no failover. While it is down, so are all previews.

## Guides it uses

- Publish a container, and publish a virtual machine.
- Move a hostname: claims, clones, and handing a hostname to another guest.
- Who may publish: the gate tag and the admission modes.
- Several tokens and accounts.
- Monitoring.
