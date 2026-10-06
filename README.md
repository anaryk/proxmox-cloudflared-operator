# pco

pco is a Cloudflare Tunnel operator for Proxmox VE. Tag a guest, list the
hostnames it should serve in its Notes, and pco publishes them through a
Cloudflare Tunnel.

## What it is

pco runs as a daemon on a Proxmox VE node. It reads the guests through the
Proxmox API, takes the ones that carry the tag `cf-tunnel`, and reads hostnames
and ports from their Notes. It then keeps Cloudflare in line with what it found:
a tunnel for each Cloudflare account, the ingress rules of the tunnel, and a
proxied DNS record for each hostname. A `cloudflared` connector for each tunnel
runs on the node as a confined systemd unit. Before pco publishes an address it
checks that the guest really answers for it, so that a guest cannot publish the
address of the node or of another machine by naming it in its Notes.

## What it is not

pco is not a reverse proxy and is never in the data path: traffic goes from
Cloudflare to the connector on the node to the guest, and does not pass through the
daemon. It does not create or change guests, and never writes their tags or Notes.
It publishes HTTP and HTTPS to IPv4 origins and nothing else: no TCP or SSH
tunnels, no WARP or private networks, and no Cloudflare Access policies. A
published hostname is public, and what protects the application behind it is the
application. pco runs on one node of a cluster and does not fail over: when that node is
down, so are its hostnames.

## What it does

- Publishes the hostnames that the Notes of tagged virtual machines and containers list,
  over HTTP or HTTPS to a port of the guest.
- Proves that an address belongs to the guest with ARP and the forwarding table of the
  bridge, and goes on watching: an address whose MAC moves is cut off at once.
- Confines each connector with an nftables filter to Cloudflare, the resolvers of the node
  and the verified addresses.
- Writes the tunnel configuration and a proxied CNAME for each hostname, changes only the
  records that carry its marker, and removes a record once its hostname has been unwanted
  for a grace period.
- Starts in observe-only mode: `pco plan` shows what would change, and nothing changes at
  Cloudflare until `pco apply`.
- Keeps hostnames apart: the first guest to claim one keeps it, an apex or a wildcard is
  published only when a setting names it, and an admission mode has an admin approve each
  guest.
- Says what it does and why, with `pco status`, `pco routes`, `pco diagnose <hostname>` and
  `pco doctor`.
- Has a web interface on the node, where a Proxmox user signs in with their own login:
  readers look at the routes, tunnels and traffic, and admins act on them.
- Installs on the node, or as an appliance, a container that holds pco and its connectors
  and leaves the node itself without them (see [Profiles](docs/profiles.md)).

[Architecture](docs/architecture.md) has the parts, the cycle and the path of a request,
with diagrams.

## What it needs

To run pco on a node, the host profile:

- Proxmox VE 8.4 or a later 8.x, which is on Debian 12, or Proxmox VE 9.x, which is on
  Debian 13, on amd64 or arm64. `pco setup` refuses any other version.
- Root on the node, to install the package and run `pco setup`.
- `cloudflared`, which runs the tunnels. `pco setup` installs it from Cloudflare's package
  repository when it is missing, and asks first; the package of pco only recommends it.
- A Cloudflare account with a domain whose zone is active, and an API token with Cloudflare
  Tunnel Edit on the account and Zone Read and DNS Edit on the zone (see
  [Cloudflare token](docs/cloudflare-token.md)).
- Outbound access from the node: port 7844, TCP and UDP, to Cloudflare's edge for the
  connectors, and port 443 for the API.
- Guests with an IPv4 address that pco can learn, on a bridge where the node has an IPv4
  address too (see the [Quickstart](docs/quickstart.md) for the whole list).

The appliance installs nothing on the node and carries its own `cloudflared`; see
[Profiles](docs/profiles.md). The packages, their checksums and the signature of the
checksum file are on the [releases page][releases].

## Status

Early development. There is no release yet, and pco is not ready for production. The host
profile, the appliance profile and the web interface are in the repository.

pco runs on one node. A node of a cluster works, with the limits that
[Identity](docs/identity.md) describes for guests of the other nodes, and nothing takes over
when that node fails.

## Install

Once there is a release, the installer downloads the package, checks the signature of
the checksum file and the checksum of the package, installs it, and starts the setup:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash

That installs the host profile, pco on the node itself, which is the default. On a
terminal the script first asks, and Enter keeps it.

The other choice is the appliance: pco and its connectors in an unprivileged container,
with nothing installed on the node. Choose it explicitly with `--appliance`; the other
arguments go to the installer of the appliance, which needs the storage when the node has
more than one for containers:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash -s -- --appliance --storage local-zfs

[Appliance](docs/appliance.md) says what it creates, what it asks and what it proves less
than the host profile, and [Profiles](docs/profiles.md) sets the two side by side.

Run it as root on the node. The release key the script carries has the fingerprint
`3D326CB52862A2E91C9919EFA98A1ED57B31F91B`, and it prints which key the signature was
made with. [SECURITY.md](SECURITY.md#verifying-a-release) says how to check that key
before the script runs, and how to verify a release by hand and install the package with
`apt install ./pco_<version>_<arch>.deb`. Before the first release, `make snapshot`
builds the packages from a checkout; it needs Go and network access, so build on another
machine and copy the `.deb` to the node. Then run `pco setup`.
[Quickstart](docs/quickstart.md) walks through all of it.

## A minimal example

Add the tag `cf-tunnel` to a guest, and write this in its Notes:

~~~text
```cf-tunnel
app.example.com -> :3000
```
~~~

Then, on a node where pco is set up:

    pco plan      # what pco would change
    pco apply     # start publishing
    pco status    # the routes, tunnels and connectors

`app.example.com` now goes through the tunnel to port 3000 of the guest. A new install
only observes until `pco apply`.

## Documentation

The pages will be published, with search, at
<https://anaryk.github.io/proxmox-cloudflared-operator/>, and the package carries them in
`/usr/share/doc/pco/docs`.

- [Quickstart](docs/quickstart.md): install, set up, publish a first hostname.
- [FAQ](docs/faq.md): whether it needs root, what it changes on the node, what happens when
  Cloudflare is down, clusters, cost, and how it differs from running `cloudflared` by hand.
- [Annotations](docs/annotations.md): everything you can write in the Notes, and what is
  rejected.
- [Cloudflare token](docs/cloudflare-token.md): the permissions, how to make the token,
  and how pco names what it creates.
- [Architecture](docs/architecture.md): the parts, the cycle, the path of a request and
  the one writer.
- [Identity](docs/identity.md): how pco proves that a guest owns an address.
- [Security](docs/security.md): the threat model, the identity levels against five
  attackers, the egress filter of the connectors, and where the secrets live.
- [Operations](docs/operations.md): the daemon, the store, the cycle, exit codes and
  upgrades.
- [Profiles](docs/profiles.md): host and appliance side by side.
- [Appliance](docs/appliance.md): pco in a container of its own, its install, its rules,
  upgrades and recovery.
- [Troubleshooting](docs/troubleshooting.md): reading `pco status`, `pco doctor` and
  `pco diagnose`, and the problems that come up.
- [Uninstall](docs/uninstall.md): taking pco off a node, and recovering after a lost
  store.
- Reference: [the commands](docs/cli/index.md), [settings](docs/settings.md),
  [files](docs/files.md), [problems](docs/problems.md) and the
  [glossary](docs/glossary.md).

## Development

Go 1.26 builds it.

    make build          # bin/pco
    make test           # go test with the race detector
    make lint           # golangci-lint
    make test-scripts   # the installer and release scripts; needs shellcheck, and gpg, gpgv or sqv for the signature checks
    make snapshot       # builds the .deb packages into dist/, without signing
    make docs           # builds the documentation site; needs Node, see site/README.md
    make docs-lint      # checks the pages, their links and the images

The Makefile passes the build tag `nomsgpack`; see [Operations](docs/operations.md) for
why. How a release is made is in [packaging/RELEASING.md][releasing], which the package
does not ship, and how the site is built is in [site/README.md][site].

## Security

Report a vulnerability through GitHub's private reporting, as [SECURITY.md][security]
describes; it also has the fingerprint of the release key and how to verify a release. What
pco protects, and where each protection stops, is in [Security](docs/security.md).

## Licence

MIT, see [LICENSE][licence]. The package ships it as `/usr/share/doc/pco/copyright`.

pco is not affiliated with Proxmox Server Solutions GmbH or Cloudflare, Inc.

[releases]: https://github.com/anaryk/proxmox-cloudflared-operator/releases
[releasing]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/packaging/RELEASING.md
[security]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/SECURITY.md
[site]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/site/README.md
[licence]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/LICENSE
