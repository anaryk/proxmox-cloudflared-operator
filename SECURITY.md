# Security policy

## Reporting a vulnerability

Report a vulnerability in pco privately, through GitHub: open the Security tab of the
repository and choose "Report a vulnerability", or go straight to
<https://github.com/anaryk/proxmox-cloudflared-operator/security/advisories/new>. Only you and
the maintainers see the report. Please do not open a public issue or a pull request for it,
and do not post it anywhere, before a fix is out.

A report is easiest to act on when it says:

- the version (`pco version`), the profile (`pco status` prints `Profile:`) and the version of
  Proxmox VE;
- what you did and what happened, with the smallest Notes, settings or requests that show it;
- what an attacker needs to start with (a guest, a Proxmox user and which privileges, a
  Cloudflare token, access to the network) and what they gain.

Leave real tokens, and anything from `/etc/pve/priv/pco`, out of it.

The maintainers answer in the advisory, and say whether they take it for a vulnerability. The
fix is prepared in the advisory and released, and the advisory is published with the release
that has it. You are named in it unless you ask not to be. If you plan to publish and have a
date, put it in the report.

## What counts

pco runs as root on a hypervisor, holds tokens for Proxmox and Cloudflare, and is meant to
stop a guest from publishing what it does not own. These are vulnerabilities:

- A guest, or a user who can edit a guest, publishing a hostname or an address that
  [Security](docs/security.md) says they cannot: the identity checks, the claims, the
  denylist, the hostname policy and the admission mode.
- A connector reaching an address that the egress filter should refuse, or the filter being
  turned off or bypassed by anything but root on the node.
- Reading or changing anything through the socket or the web interface without the role the
  documentation names, and flaws in the sign-in, the sessions, the request checks or the
  certificate handling of the web interface.
- A token (Cloudflare, Proxmox or the run token of a tunnel) in an output, a log or an answer
  of the API, or readable by someone it should not be.
- Anything that lets a file that is not the release's be installed: the installer, the check
  of the signature, the packages, the appliance templates and the release process.
- Running code on the node or in the container through any of these, or through a crafted
  Notes field, request, or answer of Proxmox or Cloudflare.

These are not, because the documentation states them as limits:

- A protection that stops where [Security](docs/security.md) says it stops. At the identity
  level `observed`, for instance, a user who may set the MAC of a guest's network card can
  reach a device on the segment, and the page says so.
- What root on the node can do. The daemon runs as root and is part of the node's trusted
  base.
- What a leaked Cloudflare token with the permissions of pco's can do at Cloudflare, beyond
  what pco does about it; the page lists both.
- A published hostname being public.
- Flaws in Proxmox VE, `cloudflared` or Cloudflare. Report those to their maintainers. Tell us
  as well if the list of vetted `cloudflared` versions
  (`packaging/cloudflared-versions.json`) should deny a version.

When you are not sure which side a finding is on, report it.

## Supported versions

Fixes go into the latest release, the one marked "Latest" on the
[releases page](https://github.com/anaryk/proxmox-cloudflared-operator/releases) and the one
the installer takes. A fix is a new release, and the way to get it is to upgrade: older
releases and pre-releases (`-rc`) are not patched, and downgrades are not supported.

| Version | Supported |
|---|---|
| The latest release | Yes |
| Older releases, and pre-releases | No; upgrade to the latest |
| The `main` branch | It is not a release; a fix lands there first |

There is no release yet. Until the first one, only the current state of `main` is looked at.

The appliance template carries Debian's packages of the day it was built, and the container
takes Debian's security updates itself. A workflow compares the template with Debian's
current packages every month, and when something changed the maintainers release a new
template as a patch version.

## Verifying a release

Every release publishes `checksums.txt`, which lists the sha256 of each package, appliance
template, SBOM and the other files of the release, and `checksums.txt.sig`, a detached
signature of it. The signature is made with one release key:

| | |
|---|---|
| User ID | `pco release signing key <tomas.marek@computer-solutions.cz>` |
| Type | ed25519 |
| Fingerprint | `3D326CB52862A2E91C9919EFA98A1ED57B31F91B` |
| `gpg` prints it as | `3D32 6CB5 2862 A2E9 1C99  19EF A98A 1ED5 7B31 F91B` |

The private key is used only by the release workflow, after a maintainer has approved that run
in a protected environment; the jobs that run npm or build the templates never hold it. See
[packaging/RELEASING.md](packaging/RELEASING.md). The public key is in `scripts/install.sh`, and
the package carries it as `/usr/share/pco/release-key.gpg`.

### With the installer

`scripts/install.sh` carries the key. It downloads the package, `checksums.txt` and
`checksums.txt.sig`, checks the signature, then the checksum of the package, and installs
nothing before both pass. Before it installs, it prints the key the signature was made with:

    verified by: signature by key 3D326CB52862A2E91C9919EFA98A1ED57B31F91B and checksum

If the fingerprint is another one, or the line says `checksum only (signature check skipped)`,
which `PCO_INSECURE_SKIP_SIGNATURE=1` does, stop. A signature that does not verify, or a key
that has expired or was revoked, makes it stop by itself. Read the script before you run it as
root; it is short. [Quickstart](docs/quickstart.md) has the commands.

### By hand

1. Download `pco_<version>_<arch>.deb`, `checksums.txt` and `checksums.txt.sig` from the
   release. For an appliance add `pco-appliance_<version>_<arch>.tar.zst`.
2. In a checkout of the repository at the tag of the release, write the keyring of its
   `scripts/install.sh` and print the fingerprint of each key in it:

       packaging/release-key.sh scripts/install.sh release.gpg

   One of them must be `3D326CB52862A2E91C9919EFA98A1ED57B31F91B`. While one release key
   replaces another, a second may stand beside it. Compare with this page as it is on GitHub,
   and not with the copy in the checkout that you are verifying.
3. Check the signature:

       gpgv --status-fd 1 --keyring ./release.gpg checksums.txt.sig checksums.txt

   It is good when the output has a `GOODSIG` line and a `VALIDSIG` line that ends in the
   fingerprint, and none of `BADSIG`, `ERRSIG`, `REVKEYSIG`, `EXPKEYSIG` and `EXPSIG`. Do not
   go by the exit status alone: `gpgv` exits 0 for a signature by a key that has expired or was
   revoked. `sqv --keyring ./release.gpg checksums.txt.sig checksums.txt`, which Proxmox VE 9
   has in place of `gpgv`, refuses such a key itself and prints the fingerprint.
4. Check the checksum of each file you downloaded:

       sha256sum --check --ignore-missing checksums.txt

5. Install the package: `apt install ./pco_<version>_<arch>.deb`.

`pco appliance install --checksums checksums.txt` checks the template that Proxmox downloads
against the same file. Run it with the `checksums.txt` you verified.

### If the key changes

A new key goes into `scripts/install.sh` beside the old one first, and a release is signed with
the old key while both are there; the fingerprints in this file, the README and the quickstart
change when the old one goes. An installer of an old release carries only the old key.

## Reading further

[Security](docs/security.md) is the threat model: what runs with which rights, what each
protection stops, and where the secrets are kept.
