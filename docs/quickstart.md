# Quickstart

This page takes a Proxmox VE node from nothing to one published hostname. Every
command runs as root on the node.

pco has no release yet, so the install section below says what to do before there is one.

## Before you start

You need:

- Proxmox VE 8.4 or later, or 9.x, on amd64 or arm64.
- A domain whose DNS is on Cloudflare, with the zone active.
- A Cloudflare API token that can edit tunnels and DNS records in that zone.
  [Cloudflare token](cloudflare-token.md) says which permissions and how to make
  it. You can create it after the install.
- Outbound access from the node: the connectors open connections to Cloudflare's
  edge on port 7844 (TCP and UDP), and pco calls the Cloudflare API on port 443.

And these on the guests:

- The node reaches a guest as itself, so the node needs an IPv4 address on the
  bridge (and VLAN) the guest's network card is attached to. On a default install
  that is `vmbr0` and the management address. [Identity](identity.md) explains why.
- The guest has an IPv4 address that pco can learn. For a container nothing more is
  needed. For a virtual machine the QEMU guest agent must run in the guest and be
  enabled in its options, or the route must name the address (see
  [Annotations](annotations.md)), or the address must be set in the guest's
  Proxmox configuration (cloud-init `ipconfig`).
- The service listens on that address, not only on 127.0.0.1.

pco runs on one node. On a cluster, install it on the node whose guests you want
to publish; the guests of other nodes need a setting described in
[Identity](identity.md) and the [profiles](profiles.md) page says what else is
missing for clusters.

## Install

Once there is a release, the installer downloads the package of your architecture,
checks the signature of the checksum file against the release key it carries, checks
the checksum of the package, installs it with apt and starts `pco setup`:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash

Read a script before you pipe it into a shell: `scripts/install.sh` is short, and
it is the one thing in this chain that you have to trust. Arguments after `bash -s --`
go to `pco setup`, so `bash -s -- --yes` takes every default. `PCO_VERSION=1.2.3`
installs a given release instead of the latest. The installer needs `curl`,
`sha256sum`, `base64`, `apt-get` and either `gpgv` or `sqv`; a Proxmox VE node has
them, or `apt-get install gpgv` adds the one that is missing.

The manual way is to install the package yourself. From a release, download
`pco_<version>_<arch>.deb` and `checksums.txt` from the releases page of the
repository, compare the checksum, and install:

    sha256sum --check --ignore-missing checksums.txt
    apt install ./pco_<version>_<arch>.deb

`checksums.txt` comes with a detached signature, `checksums.txt.sig`. A checksum
that arrives from the same place as the package proves nothing by itself, so
check the signature as well, against the release key in `scripts/install.sh`
(`packaging/release-key.sh scripts/install.sh release.gpg` writes the keyring and
prints the fingerprints, and `gpgv --keyring ./release.gpg checksums.txt.sig
checksums.txt` checks it).

Before the first release there is nothing to download. Build the packages from a
checkout of the repository instead:

    make snapshot
    apt install ./dist/pco_*_amd64.deb

`make snapshot` needs Go and network access and builds the packages without
signing them. Use the file that matches the architecture of the node.

The package installs `/usr/bin/pco`, three systemd units, a `sysusers.d` file for
the system user `pco-connector`, and depends on `nftables`. It does not enable or
start anything. Setup does that.

## Set up the node

    pco setup

Setup looks at each thing before it changes it, so running it again finishes a
run that stopped and changes nothing on a node that is set up. In order, it:

- checks that this is Proxmox VE 8.4 or later (or 9.x), that `/etc/pve` is
  mounted, and the architecture;
- makes the store (see [Operations](operations.md)) and the identity of this
  install, a random 12 character id, in observe-only mode;
- creates the Proxmox role `PCO`, the user `pco@pve`, grants the role to the user
  on `/`, and makes the API token `pco@pve!pco`. The role is read-only: it holds
  `VM.Audit`, `Sys.Audit`, `SDN.Audit`, and `VM.GuestAgent.Audit` on Proxmox VE 9
  or `VM.Monitor` on 8.4;
- registers the tags `cf-tunnel` and `cf-tunnel-managed` as registered tags, so
  that only a user with `Sys.Modify` on `/` can set them on a guest (it asks;
  `--no-registered-tags` declines);
- installs `cloudflared` from Cloudflare's apt repository if it is missing (it
  asks; `--skip-cloudflared` declines);
- asks for a Cloudflare token, checks it and stores it if it can do what pco needs;
- registers the node and enables and starts `pco.service`.

Everything it creates is written down in `/var/lib/pco/manifest.json`, which is
what [uninstall](uninstall.md) follows. A run looks like this (the versions and the
id will differ):

    preflight: Proxmox VE 9.0.10 on amd64
    store: created install 7f3a9c0d41b2; it only observes until pco apply
    role PCO: created with VM.Audit, Sys.Audit, VM.GuestAgent.Audit, SDN.Audit
    user pco@pve: created, granted role PCO on /
    token pco@pve!pco: created
    registered tags: added cf-tunnel, cf-tunnel-managed
    note: clones and restores of a guest keep its tags, so the clone of a published guest asks to be published too
    cloudflared: installed from https://pkg.cloudflare.com/cloudflared
    credentials: skipped; add one later with pco credential add --label <label>
    node registry: registered pve1
    pco.service: enabled and running
    next: tag a guest with cf-tunnel and write its routes into its notes, then run pco plan to see what would change and pco apply to make it so

The questions it asks go to the terminal. Without one, pass `--yes` to take the
default answer to every question. The token is never taken from an argument or the
environment: use `--cf-token-file <file>` or `--cf-token-stdin`, or leave it for
the next step.

A fresh install only observes. It reads the guests and works out what it would
do, and changes nothing at Cloudflare until you say so with `pco apply`.

## Add a Cloudflare token

Create the token as described in [Cloudflare token](cloudflare-token.md), then give
it to the daemon. The token is asked for without being shown; `--token-file <file>`
or standard input work in a script:

    pco credential add --label main

The check lists what the token can do. A real run prints something like this:

    Credential main (a1b2c3d4)
    Token:     active, expires 2026-10-13T14:00:00+02:00
    Accounts:  Main
    Zones:     example.com (active)

      ✓ token
      ✓ accounts
      ✓ zones
      ✓ dns.read on example.com
      ✓ tunnel.read on Main

    Usable:    yes
    Write access was not tried. Run pco credential check a1b2c3d4 --deep to prove it.

    Added credential a1b2c3d4 (main).

`credential add` only reads. Whether the token may write is not visible to anyone
but Cloudflare, so prove it now rather than at the first publish:

    pco credential check a1b2c3d4 --deep

A deep check creates a test DNS record and a test tunnel, deletes them again, and
asks before it does. It needs a terminal, or `--yes`.

## Tag a guest and write its routes

Open the guest in the Proxmox web interface and add the tag `cf-tunnel`. On the
command line:

    qm set 101 --tags 'cf-tunnel'      # a virtual machine
    pct set 200 --tags 'cf-tunnel'     # a container

`--tags` replaces the tags the guest already has, so list those too, separated by
semicolons. If the tags are registered, this needs `Sys.Modify` on `/`, which root
has.

Then write the routes into the guest's Notes (the Notes panel of its Summary page).
One route per line, hostname, an arrow and a port:

    ```cf-tunnel
    app.example.com -> :3000
    ```

That publishes `app.example.com` and sends its requests to port 3000 of the guest.
A guest can serve several hostnames, to several ports, and a hostname can have
`https`, options and a different address. [Annotations](annotations.md) has the
whole grammar. A one-line form without the fence also works:

    cf-tunnel: app.example.com -> :3000

The daemon looks again every 10 seconds. To not wait, ask for a cycle:

    pco sync

## See what pco would do

    pco routes

lists every route and how it fares. In observe-only mode `active` means that pco
would serve the route, not that it does yet:

    HOSTNAME         STATE   LEVEL  SERVICE                OWNER     ZONE         NOTE
    app.example.com  active  port   http://10.0.0.11:3000  qemu/101  example.com  -

`SERVICE` is the address pco verified for the guest and the port from the Notes.
`LEVEL` says how strongly the address was proven; [Identity](identity.md) explains
`port` and `observed`. A route that is not `active` has the reason in `NOTE`.

    pco plan

lists the actions the daemon has not applied and why: in observe-only mode every
one of them is held by `observe mode`. It also lists records of someone else that
stand in the way of a hostname, and anything that waits for a confirmation.

    pco status

is the overview: the mode, whether the inventory is complete, the routes by state,
the tunnels and their connectors, the credentials, the problems in the Notes and
the problems of the daemon. While nothing is applied it ends with the next step:

    Run pco apply to start publishing.

## Publish

    pco apply

leaves observe-only mode. From the next cycle the daemon creates the tunnel
`pco-<id>` in your Cloudflare account, writes its configuration, starts a
`cloudflared` connector for it, and creates a proxied DNS record for each hostname.

    Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status.

`pco status` shows the tunnel, whether its configuration is verified, and its
connector once it has connected:

    Tunnels:
      NAME              ID        VERIFIED  CONNECTOR
      pco-7f3a9c0d41b2  0a1b2c3d  yes       active, ready, 4 connections

## Check that it works

`pco diagnose` follows one hostname through everything between a visitor and the
guest, and stops at the first step that fails:

    pco diagnose app.example.com

    ✓ route      qemu/101 (web-1) holds it; state active
    ✓ zone       zone example.com is active
    ✓ dns        its record points at the tunnel
    ✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://10.0.0.11:3000 (configuration version 4)
    ✓ connector  active, ready, 4 connections
    ✓ identity   10.0.0.11 is the address of qemu/101, verified at identity level port (reported)
    ✓ tcp        10.0.0.11:3000 answers
    ✓ http       the origin answered 200 OK

The last step is a request that the daemon makes to the guest, as the tunnel would
make it, with the Host header and the TLS settings of the route. It comes from the
daemon, not from the connector.

`pco doctor` checks the installation as a whole: the mode, the last cycle, the
credentials, `cloudflared`, the tunnels and connectors, the way out to Cloudflare,
the writer, Proxmox, the store. Every finding that is not fine comes with what to do
about it:

    ✗ cloudflared       cloudflared does not run: exec: no such file
                        fix: install cloudflared from the package repository of Cloudflare
    ! credential cred1  not checked yet
                        fix: pco credential check cred1
    ✓ mode              enforce: changes are applied
    ✓ store             the store is mounted and set up

    1 failure, 1 warning.

If something is wrong, [Troubleshooting](troubleshooting.md) starts from the
output of these commands.

## Where to go next

- [Annotations](annotations.md): everything you can write in the Notes.
- [Identity](identity.md) and [Security](security.md): what pco proves before it
  publishes an address, and what it does not.
- [Operations](operations.md): the daemon, the settings, upgrades.
- [Uninstall](uninstall.md): how to take pco off the node again.
