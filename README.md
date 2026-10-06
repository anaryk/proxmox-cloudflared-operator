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
application. There is no web interface yet, only the command line, and no cluster
support: pco runs on one node.

## How it works

1. You tag a guest `cf-tunnel` and write hostnames and ports in its Notes.
2. The daemon polls the Proxmox API, reads the Notes and works out who holds each
   hostname.
3. For each target it proves that the address belongs to the guest, with ARP and the
   forwarding table of the bridge, before anything is published, and goes on watching
   the network: an address whose MAC moves is cut off at once.
4. It writes the tunnel configuration and a proxied CNAME for each hostname at
   Cloudflare, and runs one `cloudflared` for each tunnel on the node, confined by an
   nftables filter to the targets it verified.
5. Visitors reach Cloudflare's edge, which carries the request through the tunnel to
   the node and on to the guest.

```
  visitor
     |
     v
  Cloudflare edge   <-- DNS records and tunnel configuration, written by pco
     |
     |  tunnel: outbound connection from the node
     v
  cloudflared on the Proxmox node   (pco-cloudflared@<tunnel>.service)
     |
     |  http or https, to the address pco verified
     v
  guest   10.0.0.11:3000
```

## Status

Early development. There is no release yet, and pco is not ready for production. The
first milestone is the host profile, where the daemon and the connectors run on the
node (see [Profiles](docs/profiles.md)).

Supported: Proxmox VE 8.4 or later and 9.x, on amd64 and arm64. pco runs on one node;
a node of a cluster works, with the limits that [Identity](docs/identity.md) describes
for guests of the other nodes.

## Install

Once there is a release, the installer downloads the package, checks the signature of
the checksum file and the checksum of the package, installs it, and starts the setup:

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash

On a terminal the script first asks whether to install on the node (the host profile,
which Enter keeps) or as an appliance, a container with nothing installed on the node;
`--appliance` answers that beforehand (see [Profiles](docs/profiles.md)):

    curl -fsSL https://raw.githubusercontent.com/anaryk/proxmox-cloudflared-operator/main/scripts/install.sh | bash -s -- --appliance

The release key the script carries has the fingerprint
`3D326CB52862A2E91C9919EFA98A1ED57B31F91B`, and before it installs anything it says
which key the signature was made with.

Read the script first, and run it as root on the node. The manual way is to download
`pco_<version>_<arch>.deb` and `checksums.txt` from the releases page, check them, and
install the package with `apt install ./pco_<version>_<arch>.deb`. Before the first
release, `make snapshot` builds the packages from a checkout; it needs Go and network
access, so build on another machine and copy the `.deb` to the node. Then run `pco setup`.
[Quickstart](docs/quickstart.md) walks through all of it.

## A minimal example

Add the tag `cf-tunnel` to a guest, and write this in its Notes:

~~~text
```cf-tunnel
app.example.com -> :3000
```
~~~

Then, on the node:

    pco plan      # what pco would change
    pco apply     # start publishing
    pco status    # the routes, tunnels and connectors

`app.example.com` now goes through the tunnel to port 3000 of the guest.

## Documentation

- [Quickstart](docs/quickstart.md): install, set up, publish a first hostname.
- [Annotations](docs/annotations.md): everything you can write in the Notes, and what is
  rejected.
- [Cloudflare token](docs/cloudflare-token.md): the permissions, how to make the token,
  and how pco names what it creates.
- [Identity](docs/identity.md): how pco proves that a guest owns an address.
- [Security](docs/security.md): the threat model, the identity levels against four
  attackers, the egress filter of the connectors, and where the secrets live.
- [Operations](docs/operations.md): the daemon, the store, the cycle, the settings,
  exit codes and upgrades.
- [Profiles](docs/profiles.md): host and appliance side by side.
- [Troubleshooting](docs/troubleshooting.md): reading `pco status`, `pco doctor` and
  `pco diagnose`, and the problems that come up.
- [Uninstall](docs/uninstall.md): taking pco off a node, and recovering after a lost
  store.

## Development

Go 1.26 builds it.

    make build          # bin/pco
    make test           # go test with the race detector
    make lint           # golangci-lint
    make test-scripts   # the installer and release scripts; needs shellcheck, and gpg, gpgv or sqv for the signature checks
    make snapshot       # builds the .deb packages into dist/, without signing

The Makefile passes the build tag `nomsgpack`; see [Operations](docs/operations.md) for
why. How a release is made is in [packaging/RELEASING.md][releasing], which the package
does not ship.

## Licence

MIT, see [LICENSE][licence]. The package ships it as `/usr/share/doc/pco/copyright`.

pco is not affiliated with Proxmox Server Solutions GmbH or Cloudflare, Inc.

[releasing]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/packaging/RELEASING.md
[licence]: https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/LICENSE
