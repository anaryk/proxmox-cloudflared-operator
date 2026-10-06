# The appliance template

The appliance is pco in an unprivileged Proxmox VE container of its own. Its
template, `pco-appliance_<version>_<arch>.tar.zst`, is a Debian 13 root file
system with pco and cloudflared installed; `pco appliance install` makes the
container from it. Every release carries one for amd64 and one for arm64.

## What is in it

- Debian's `minbase` variant and `packages.txt`, from the snapshot of
  snapshot.debian.org that `pin.conf` names. No SSH server, no sudo, no cron
  and no curl.
- The pco package of the release and the newest cloudflared that
  `../cloudflared-versions.json` allows, checked against the sha256 listed
  there. Both are held: `pco upgrade` changes them, apt does not.
- `overlay/`, copied onto the root: the profile marker `/etc/pco/profile`, the
  apt sources of the live archives (`deb.debian.org` trixie and trixie-updates,
  `security.debian.org` trixie-security), unattended upgrades of Debian's
  security updates, `pco-first-boot.service`, which installs those published
  since the snapshot at the first start that has a network, a journal of at most
  64 MB, the host name `pco` and a short `/etc/motd`. Its drop-ins put
  `pco.service` and the connectors behind `pco-net.service`, keep `pco.service`
  failed when it exits with 78 (no volume), and start a connector only once the
  daemon wrote `/run/pco-appliance/identity-ok`; `overlay_test.sh` checks them.
- `pco-net.service`, `pco-egress.service`, `pco.service`,
  `pco-first-boot.service` and `unattended-upgrades.service` are enabled,
  `nftables.service` is masked (it would flush the tables of pco), the root
  password is locked and `/etc/machine-id` is empty.

Nothing of the build stays: the snapshot sources, the apt option that let apt
read their old Release files, the resolver and the logs of the host are taken
out by the last step of the build. `pco-appliance_<version>_<arch>.spdx.json`
lists the packages of a template as SPDX 2.3.

## Building it

    make snapshot        # the packages into dist/, and the amd64 template
    make template        # the template of the package in dist/
    make template TEMPLATE_ARCH=arm64

The template lands in `build/appliance`. `build.sh` runs mmdebstrap in unshare
mode, which needs Linux, `mmdebstrap`, `zstd`, `jq`, `curl` and `dpkg`, and
either root or a user with a range in `/etc/subuid` and `/etc/subgid` (the
`uidmap` package) on a kernel that lets users make user namespaces; Ubuntu
24.04 forbids that until `sysctl kernel.apparmor_restrict_unprivileged_userns=0`.
Debian's keyring comes with the build rather than from the host (see the
snapshot below), so the host may be Ubuntu as well as Debian. The architecture
the host is not needs `qemu-user-static` and binfmt. Elsewhere, run it as root in a
Debian container, after `make snapshot` on the host:

    docker run --rm --privileged -v "$PWD:/src" -w /src debian:trixie sh -c \
      'apt-get update && apt-get install -y make mmdebstrap zstd jq curl ca-certificates && make template'

`build.sh --dry-run` prints the mmdebstrap command without running anything,
on any system.

## The same bytes twice

Two builds from the same `pin.conf` and the same packages give the same files
on the same host. Every file of the template carries the time of the snapshot
(also the `SOURCE_DATE_EPOCH` of the build), mmdebstrap writes the tar sorted by
name with numeric owners, the overlay goes in as root's with fixed modes, and
zstd writes the same output for the same input and options. `build_test.sh`
builds twice and compares the sha256 of every file; the `template` job of CI
runs it. The promise holds within one runner image: the mmdebstrap and zstd of
the host take part in the bytes, and the versions Ubuntu ships change between
images, so a build on another runner, or a month later, may differ while
carrying the same packages.

A release builds the packages twice: once in its job `build`, which makes the
templates from them, and once in its job `sign`, which signs. The templates do
not keep the package they installed. `build.sh` writes its sha256 into
`pco-appliance_<version>_<arch>.deb.sha256`, which `check-artifacts.sh
--require-template` looks at first; then it reads `/usr/bin/pco` out of each
template and out of the package the job `sign` built, and refuses the release
unless they are the same bytes. It does the same for `/usr/bin/cloudflared`,
against the cloudflared package the job `build` keeps beside the templates,
once that package has the sha256 `../cloudflared-versions.json` lists. The
`package` job of CI builds the packages in two checkouts and compares them, so
that a change that breaks this fails there first.

## The snapshot

`pin.conf` names the snapshot as `SNAPSHOT=YYYYMMDDTHHMMSSZ`.
`build.sh --snapshot` takes another one, and so does the release workflow, from
a line `Snapshot: YYYYMMDDTHHMMSSZ` of the tag's message or from its input
`snapshot`. Each release carries the one it was built from as
`pco-appliance_<version>.pin.conf`. The workflow `template` builds the
templates of the latest release from the current snapshot every month and,
when their packages changed, opens an issue that asks for the tag of a patch
release with that snapshot (see `../RELEASING.md`).

apt checks the signatures of the snapshot against Debian's keyring and nothing
the host trusts. `KEYRING_VERSION` and `KEYRING_SHA256` in `pin.conf` name the
`debian-archive-keyring` package of trixie; `build.sh` downloads it from the
snapshot of `pin.conf`, also when it builds from another one, refuses it unless
it has that sha256, and hands its `debian-archive-keyring.pgp` to mmdebstrap
with `--keyring`. Moving `SNAPSHOT` to a time when trixie has another version
of the package means moving these two as well, to the version and the SHA256
that `dists/trixie/main/binary-all/Packages.xz` of the snapshot lists; the
signature of its `InRelease` covers that file. A snapshot signed by a key the
pinned keyring does not have fails to build until they move. `pin_test.sh`,
which `make test-scripts` runs, checks what `build.sh` takes from `pin.conf`.

## Testing it

    sudo packaging/appliance/smoke.sh --version <version> build/appliance/pco-appliance_<version>_amd64.tar.zst
    sudo packaging/appliance/smoke.sh --with-network --version <version> <template>

`smoke.sh` first reads the files of the template: `/etc/machine-id` must be
empty, `/etc/resolv.conf` absent, `/root/.ssh` absent or empty (the package
systemd makes the directory, but no key may be in it), and the dpkg database
must list neither `openssh-server`, `sudo`, `cron` nor `curl`. Then it boots the
template in `systemd-nspawn` and checks it from inside: without a network it
must come up running, or degraded by nothing but `pco.service`,
`pco-first-boot.service` and `pco-net.service`; pco must be the version of the
file name, the egress table loaded, `pco net show` must say what
`pco-net.service` found, the drop-ins must be in force, the profile
`appliance`, pco and cloudflared held, root
locked, `pco-connector` there, `nftables.service` masked, apt must read the
two live archives and nothing of the snapshot, and `unattended-upgrade
--dry-run -v` must allow the origins labelled `Debian-Security` and no other.
With `--with-network` it boots once more with the network of the host, and the
first start must have installed the security updates and left the package
lists of the live archives only. For a snapshot, whose binary says what
`git describe` says, add `--pco-version "$(git describe --tags --always --dirty)"`.
It needs root and `systemd-container`, on a host whose kernel has the
nftables modules pco loads (`nft_fib_inet` among them).
