# How-to guides

Each guide is one task, from start to end: when you need it, what to have ready, the steps with
the commands and the Notes to write, how to check the result, and how to undo it. The guides
under the first three headings are for a node of the host profile, set up as in
[Quickstart](../quickstart.md); the last four are for the appliance. They link the concept pages
and the reference where those say more.

## Publishing

- [Publish a container](publish-a-container.md): the gate tag, the Notes, `pco plan`,
  `pco apply`, and the check.
- [Publish a virtual machine](publish-a-vm.md): the guest agent or a static address, several
  hostnames and ports, a second network card with `via=`.
- [Publish an HTTPS origin](https-origins.md): `https` targets, `no-tls-verify`, `sni=` and
  `host-header=`.
- [Publish a wildcard](wildcards.md): wildcard routes, `allowHosts`, and the order in which
  rules match.
- [Publish a route by hand](manual-routes.md): manual routes, `manualCIDRs` and `allowNode`.
- [Move a hostname to another guest](move-a-hostname.md): claims, handing a hostname over,
  and clones of a published guest.
- [Take over existing DNS records](adopt-existing-records.md): from a `cloudflared` of your
  own or an address record, with `pco adopt`.

## Reaching guests the node is not on

- [Attach a network](attach-a-network.md): the candidates, the probe, the uplink warning and
  a leg of pco's own in a bridge or VLAN, without touching the guests.
- [Set up the managed network](managed-network.md): the SDN vnet `pconet`, the consent of a
  guest, the card pco adds, DHCP for a virtual machine, and automatic consent.

## Deciding and securing

- [Roll out in observe-only mode](observe-then-enforce.md): reading `pco plan` before
  `pco apply`, and going back.
- [Decide who may publish](who-may-publish.md): the gate tag, the hostname policy, admission
  mode `approve` and approvals, and pools.
- [Use several tokens and accounts](several-tokens-and-accounts.md): credentials, zones per
  token, zone pins and the check of a token.
- [Work with the egress filter](egress-filter.md): what it allows, `pco egress show`, the block
  list and the off switch, and what a rejection looks like.
- [Rotate the tunnel token](rotate-the-tunnel-token.md): `pco tunnel rotate`, connectors that
  are not pco's, and what visitors notice.

## Running pco

- [Run pco on a cluster](clusters.md): which node to install on, guests of other nodes and
  their segments, and what stops when that node is down.
- [Upgrade and go back](upgrade-and-rollback.md): pco and `cloudflared`, held packages, and
  earlier versions.
- [Back up and recover](backup-and-recovery.md): what to back up, a lost store, a restored node
  or guest, and moving pco to another node.
- [Monitor pco](monitoring.md): `pco doctor` from a timer, `pco status --json`, events, the
  journal and the metrics of the connectors.

## The appliance

The same tasks for [the appliance](../appliance.md), pco in a container of its own.

- [Install the appliance](appliance-install.md): the installer and its questions, the segment,
  the approvals, and what `pco doctor` shows on a fresh install.
- [The managed network of the appliance](managed-network-appliance.md): the vnet made and
  granted from the node, the grant per guest or per pool, the guest firewall and the level
  `filtered`.
- [Upgrade the appliance](appliance-upgrade.md): `pco upgrade --check`, the snapshot taken on the
  node before it, and the upgrade of pco and `cloudflared`.
- [Restore the appliance from a backup](appliance-restore.md): the container restored with
  `pct restore`, and its install brought back with `pco appliance repair --recover`.
