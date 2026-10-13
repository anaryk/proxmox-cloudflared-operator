# The managed network of the appliance

This guide gives an [appliance](../appliance.md) the managed network: the SDN vnet `pconet`
made and granted from the node, taken into the store inside the container, and a guest put on
it with a card the appliance adds and isolates with the guest's Proxmox firewall. The
appliance in the examples is the container `lxc/120` on node `node1`, and the guest is the
container `lxc/203`, named `moved-3`. [Set up the managed network](managed-network.md) is the
guide for a host, and what the two share is said there once.

## When you need it

A guest should be published and sits on no segment the appliance has a card on, and attaching
its network is not what you want; or the guests should be reached at the level `filtered`,
which the appliance reaches on the managed network alone: a proof at `observed` through the
leg, raised while the guest firewall of every card on `pconet` is proven to pin the card to
its address and keep it from everything but pco's leg. [Identity](../identity.md#filtered-in-the-appliance)
says what that proof rests on.

## Before you start

- The appliance is installed and runs; [Install the appliance](appliance-install.md).
- The node's firewall runs on the iptables backend, which is the default: `nftables: 1` in the
  node's firewall options is refused, since the nftables preview installed no guest rules
  where it was tried. The datacenter firewall is on, with rules that keep your own access,
  and `pve-firewall` runs on the node. The appliance's isolation rests on all three.
- `managed.subnet` of the appliance's settings, `10.203.118.0/24` by default, is free, as for
  a host.
- The SDN configuration has no changes of others waiting, or you mean to apply them along.
- You work as root on the node for the first step, with the `pco` of the release unpacked
  there ([Commands on the node](../appliance.md#commands-on-the-node)), and inside the container or with `pct exec 120 --` for the rest.

## Steps

1. On the node, make the vnet and grant it. The command reads the node's firewall first and
   refuses before anything is made when it cannot isolate the guests; then it shows what it
   makes and grants, and asks once:

   ```sh
   pco appliance managed enable --vmid 120
   ```

   ```text
   pco appliance managed enable makes the SDN zone pco and the vnet pconet where they are missing, applies the SDN configuration, and grants pco@pve and pco@pve!vm120:
     role PCOManaged (VM.Config.Network) on /vms/120
     role PCOSDN (SDN.Use, SDN.Audit) on /sdn/zones/pco/pconet
     pveum acl modify /vms/120 --users pco@pve --tokens 'pco@pve!vm120' --roles PCOManaged
     pveum acl modify /sdn/zones/pco/pconet --users pco@pve --tokens 'pco@pve!vm120' --roles PCOSDN
   Make the managed network for lxc/120 and grant these? The SDN apply reloads the network on every node. [y/N] y
   SDN: created zone pco
   SDN: created vnet pconet
   SDN: applying zone pco (new), vnet pconet (new)
   SDN: applied; the network of every node is reloaded
   granted: /sdn/zones/pco/pconet
   the managed network is made for lxc/120: vnet pconet in zone pco, granted to pco@pve!vm120
   next: pct exec 120 -- pco network managed enable
   ```

   The vnet is made without isolated ports: the appliance's own card is a port of it, and the
   guest firewall does the isolating. The grant is the one `pco appliance grant-network` makes
   for any network, the role `PCOManaged` (`VM.Config.Network`) on the appliance for its own
   card and the role `PCOSDN` (`SDN.Use` and `SDN.Audit`) on the vnet, for the user and the
   token both. The zone and the vnet are noted in the
   appliance's manifest and in the description of the container before they are made, so
   that `pco appliance uninstall` can tell them from an admin's.

2. Inside the appliance, take the network into the store:

   ```sh
   pct exec 120 -- pco network managed enable
   ```

   The daemon checks that the vnet is there as made for an appliance, stores the network
   with the lowest free mark, and adds the appliance's own card on `pconet` in its next cycle,
   `net1` named `leg1`, at the first address of the subnet.

3. Let the appliance change the guest. Its token may change only the guests it holds
   `VM.Config.Network` on, so a consenting guest waits with `needs-grant` until it does, and
   `pco managed status` prints the line:

   ```sh
   pct set 203 --tags 'cf-tunnel;cf-tunnel-managed'
   pct exec 120 -- pco managed status
   ```

   ```text
   Attached:
     GUEST              NIC   ADDRESS       MAC                CONSENT  STATE        SINCE
     lxc/203 (moved-3)  net1  10.203.118.2  06:50:43:4f:76:02  tag      needs-grant  2026-10-01T09:00:00+02:00
     lxc/203: pveum acl modify /vms/203 --users pco@pve --tokens 'pco@pve!vm120' --roles PCOManaged
   ```

   Run the line on the node. The appliance reads the access control every minute, and the
   card follows at the next read. A pool saves the line per guest: `pco appliance managed
   enable --vmid 120 --pool web` grants `PCOManaged` on `/pool/web` once, and every guest in
   that pool is writable, now and later. Read what that gives away first: the token can then
   change the network cards and the firewall of every guest of the pool, and whoever may put
   a guest into the pool hands it over too ([Security](../security.md#its-rights-in-proxmox)).

4. Confirm the firewall impact where there is one. The appliance turns the guest firewall on
   for the card it adds; a guest whose firewall is off and whose other card has `firewall=1`,
   which the GUI sets by default, would get that card filtered too. Such a guest waits:

   ```text
   Waiting:
     lxc/204 (gui-4): needs-firewall-confirmation: turning on the firewall of lxc/204 (gui-4) also filters net0 (firewall=1); run pco managed approve lxc/204 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest
   ```

   The flag confirms it for that guest, with an approval; the setting
   `managed.acceptFirewallImpact: true` confirms it for every guest attached by the tag or by
   `auto`, and the change is an event that names who made it. pco turns the firewall on with
   accepting policies, so the other card keeps passing what it passed, and never sets the
   guest-wide `ipfilter` option, which would break a card on DHCP.

5. Wait for the next cycle, and look at the card and its firewall:

   ```text
   Attached:
     GUEST              NIC   ADDRESS       MAC                CONSENT  STATE     SINCE
     lxc/203 (moved-3)  net1  10.203.118.2  06:50:43:4f:76:02  tag      attached  2026-10-01T09:00:00+02:00
   Filtered:    holds (pve-firewall)
   ```

   On the node, `cat /etc/pve/firewall/203.fw` shows `enable: 1` with accepting policies,
   the ipset `ipfilter-net1` that holds exactly the card's address, and pco's rules on top,
   commented `pco:<install id>`: in from the leg, the DHCP replies and the published ports,
   nothing else; out, the card's own address to the leg and DHCP requests, nothing else. The
   card is added with its link down, and comes up once its firewall has been written for 15
   seconds and reads back as written ([the enforcement window](../networking.md#the-appliance-the-guest-firewall));
   `Filtered` says `does not hold` with `not yet enforced` meanwhile, and the routes on
   `pconet` are at `observed` for that cycle.

6. Publish the guest as any other. Its route is served at `filtered` while the isolation
   holds:

   ```sh
   pct exec 120 -- pco routes
   ```

   ```text
   HOSTNAME           STATE   LEVEL     SERVICE                 VIA      OWNER    ZONE         NOTE
   moved.example.com  active  filtered  http://198.18.1.0:3000  managed  lxc/203  example.com  -
   ```

   With `identityMinimum: filtered` in the settings, the routes on the managed network are
   the only ones served, and every other route of the appliance is held back; with
   `observed`, which the appliance starts with, they are served at either level.

## Check

`pco doctor` in the appliance has `filtered pconet`, the isolation as the last cycle proved
it with every reason when it does not hold; `managed firewall`, whether the node can isolate
the guests at all; and `pool grant <owner>`, a note for each guest of a granted pool that has
no consent. On the node, as root, the rules in force, which the appliance cannot read itself:

```sh
iptables-save -t filter | grep -A8 'veth203i1-IN'
```

A canary runs each cycle in the appliance's place: a TCP SYN from the leg to the guest's
managed address on a port no route publishes, which must meet silence; an answer of any kind
drops `filtered` with `the firewall is not enforcing on net1`. From a managed guest, the
leg answers DHCP and nothing else, and no other guest, the node or anything beyond is reached;
unlike on a host, the other guests' MACs are seen, since the ports are not isolated there.

What takes the isolation away, and what happens then: the node's firewall switched to the
nftables backend, the datacenter firewall turned off or `pve-firewall` stopped. No card is
added while it lasts, the cards that exist stay, and their routes drop to `observed`, with
the problem line `the managed network cannot run on this node: <why>`.

## Undo

As on a host: take the consent away, and the card goes after the grace. The appliance takes
pco's rules and ipset off the card first, and turns the guest firewall off again only when
it turned it on and nobody changed its policies or added rules since; otherwise it stays on
and the event says why. `pco appliance uninstall --vmid 120 --detach-managed` on the node
removes the cards the appliance attached before it removes the container, the grants and the
SDN objects; without the flag it refuses while a guest has a card on `pconet`. See
[Uninstall](../uninstall.md#the-appliance).

## Read on

- [Networking](../networking.md#the-appliance-the-guest-firewall): what the appliance's isolation rests on,
  the enforcement window, an admin's rule above pco's, and a partial write.
- [Identity](../identity.md#filtered-in-the-appliance): the level `filtered`.
- [Security](../security.md#the-managed-network-against-the-five-attackers): what it stops.
- Reference: [pco appliance managed enable](../cli/pco-appliance-managed-enable.md),
  [pco network managed enable](../cli/pco-network-managed-enable.md),
  [pco managed status](../cli/pco-managed-status.md).
