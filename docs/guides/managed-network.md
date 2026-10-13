# Set up the managed network

This guide gives pco a network of its own on a host, the SDN vnet `pconet`, and puts a
guest on it with a card pco adds, so that a guest on a network the node is not on is reached
all the same. The guest in the examples is the container `lxc/201`, named `new-1`, on an
internal bridge of its own, and the virtual machine `qemu/105`, named `batch-5`, which gets
its address from pco's DHCP server. [The managed network of the appliance](managed-network-appliance.md)
has what differs there.

## When you need it

A guest should be published and sits on no network the node is on, or on none you want the
node or a leg in: a guest of a tenant, a guest on an internal bridge, a guest that has no
network card at all yet. The managed network reaches it through a card that exists for
publishing and nothing else, with an address pco keeps, on a vnet whose ports are isolated
from each other, so that the card adds no way between the guest and anything but pco's leg.
[Networking](../networking.md#which-way-to-a-guest) says when to attach a network instead.

## Before you start

- pco is set up on the node; `pco setup --managed` on a new node makes the network with the
  rest.
- `managed.subnet`, `10.203.118.0/24` by default, is a prefix that no node routes, no guest
  has an address in and no attached network uses; pco checks that before it makes the
  network, and the subnet cannot be changed once the network exists. Set it first in the
  settings if the default is taken: [Settings](../settings.md#managed-network).
- The SDN configuration has no changes of others waiting to be applied, or you mean to apply
  them along: the apply reloads the network of every node of the cluster.
- The tag `cf-tunnel-managed` is registered, which setup does unless told otherwise; `pco
  setup --repair` registers it later.
- You work as root on the node.

## Steps

1. Make the network. It takes the lock of the SDN configuration, makes the zone and the vnet,
   and asks before the apply:

   ```sh
   pco network managed enable
   ```

   ```text
   SDN: created zone pco
   SDN: created vnet pconet
   SDN: applying zone pco (new), vnet pconet (new)
   Apply the SDN configuration? It reloads the network on every node. [y/N] y
   SDN: applied; the network of every node is reloaded
   network pconet: stored by the daemon, which puts its leg on it in its next cycle
   ```

   On a node with no network attached yet it also enables and starts `pco-netns.service`,
   which brings the gateway namespace back at boot. Changes of others in the SDN refuse the
   command, `the SDN has changes that are not applied and are not pco's: ...`; apply or roll
   them back, or pass `--confirm-sdn-apply` to apply them along. `--yes` answers the
   question in a script. The next cycle puts pco's leg on the vnet, at the first address of
   the subnet:

   ```sh
   pco managed status
   ```

   ```text
   Mode:        per-guest
   Tags:        cf-tunnel-managed is registered admin-only
   Vnet:        pconet as pco made it
   Subnet:      10.203.118.0/24, 253 addresses free
   Leg:         10.203.118.1, up
   Attached:    none
   ```

2. Give a guest its consent. The simplest is the tag, which only an admin can set while it is
   registered; it goes beside the gate tag:

   ```sh
   pct set 201 --tags 'cf-tunnel;cf-tunnel-managed'
   ```

   An approval is the other way, and the one a clone does not inherit, since it is of the
   identity the guest has now:

   ```sh
   pco managed approve lxc/201
   ```

   `pco managed revoke lxc/201` takes it back. A guest whose Notes say
   `cf-tunnel-managed=off` on a line of its own gets no card either way.

3. Wait for the next cycle. The container has its card within seconds, with a static
   address of the subnet:

   ```sh
   pco managed status
   ```

   ```text
   Mode:        per-guest
   Tags:        cf-tunnel-managed is registered admin-only
   Vnet:        pconet as pco made it
   Subnet:      10.203.118.0/24, 252 addresses free
   Leg:         10.203.118.1, up
   Attached:
     GUEST            NIC   ADDRESS       MAC                CONSENT  STATE     SINCE
     lxc/201 (new-1)  net1  10.203.118.2  06:50:43:4f:76:02  tag      attached  2026-10-01T12:00:00+02:00
   ```

   ```sh
   pct config 201 | grep '^net1'
   ```

   ```text
   net1: name=eth1,bridge=pconet,firewall=0,hwaddr=06:50:43:4f:76:02,ip=10.203.118.2/24,type=veth
   ```

   The card's MAC is `06:50:43:4f:` and the last two octets of its address, so that the two
   belong together. The event says the same: `lxc/201 (new-1) is attached to the managed
   network: net1 at 10.203.118.2, by tag`.

4. For a virtual machine, make the guest ask for its address. pco adds a virtio card and
   answers its DHCP request with the lease, and nothing else: no router and no DNS, so the
   guest keeps its own default route. A virtual machine without network hotplug takes the card
   at its next restart, and `pco managed status` shows `pending-reboot` until then. Inside the
   guest the card is one more interface, `ens19` or `ens20` with predictable names, which
   `ip link` shows by its MAC; on a Debian guest with `ifupdown`:

   ```text
   allow-hotplug ens19
   iface ens19 inet dhcp
   ```

   [Networking](../networking.md#dhcp-for-virtual-machines) has the lines for
   `systemd-networkd`, netplan, NetworkManager and cloud-init. A running virtual machine that
   has had its card for a minute without asking is `awaiting-dhcp`, and `pco doctor` warns.

5. Publish the guest as any other: the gate tag it carries already, and its routes in its
   Notes. pco proves the address pco leased it first, through the leg, and the route says so:

   ```sh
   pco routes
   ```

   ```text
   HOSTNAME         STATE   LEVEL  SERVICE                 VIA      OWNER    ZONE         NOTE
   new.example.com  active  port   http://198.18.1.1:3000  managed  lxc/201  example.com  -
   ```

   A route can say `via=managed` to use that card and no other of the guest's;
   [Annotations](../annotations.md#options) has the option. `pco status` carries the summary,
   `Managed: per-guest, 1 attached`.

## Automatic consent

With many guests, `managed.consent: auto` attaches every guest the gate tag admits that
needs the network, without a tag or an approval each. It rests on the gate tag being
registered as admin-only, so that nobody but an admin decides which guests pco reads at all;
the setting is refused otherwise. In the settings file, through `pco settings apply`:

```json
"managed": {
  "consent": "auto"
}
```

A guest is then attached when it needs the network, has at least one valid route, is admitted
in admission mode `approve`, is on this node, and does not refuse with `cf-tunnel-managed=off`;
[Networking](../networking.md#consent) has the list, and `pco managed status` names under
`Waiting` what a guest still lacks. A guest on a bridge the node is on does not need the
network and is left alone. When the gate tag loses its registration, automatic attaches stop
and the guests that are attached stay attached; `pco setup --repair` registers it again.

## Check

`pco doctor` has a check for each part: `managed`, `vnet`, `pconet` and `dhcp`, `leg pconet`,
and `managed guest <owner>` for every card that is not plainly attached. On the node, as root:

```sh
nft list table bridge pco_guard
bridge fdb show br pconet | grep sticky
```

The guard lists each managed guest's port, MAC and address, the only triple it admits from
that port; the sticky entries keep each MAC on its guest's port, and on the bridge end of
pco's leg while the guest is stopped. From a managed guest, `10.203.118.1` answers ARP, DHCP
and nothing else, and another managed guest, the node and anything beyond the vnet are out of
reach: the ports of the vnet are isolated, the node drops what `pconet` sends it, and the leg
accepts nothing from a guest. [Networking](../networking.md#the-host-isolated-ports-and-the-guard)
says what each of these does.

## Undo

Take the consent away, and the card goes after the grace, ten minutes by default
(`managed.detachGrace`), so that a consent given back in time keeps it:

```sh
pct set 201 --tags 'cf-tunnel'
```

```text
Attached:
  GUEST            NIC   ADDRESS       MAC                CONSENT  STATE      SINCE
  lxc/201 (new-1)  net1  10.203.118.2  06:50:43:4f:76:02  tag      detaching  2026-10-01T12:00:00+02:00
  lxc/201: goes at 2026-10-01T14:10:00Z: the guest has neither the tag cf-tunnel-managed nor an approval
```

When the grace ends the card is removed, the lease is released once the configuration no
longer lists the card, and the attachment is forgotten: `lxc/201 (new-1) is detached from the
managed network; 10.203.118.2 is free again`. A virtual machine without network hotplug drops
its card at its next restart and is `detach-pending` until then.

The network itself goes with `pco uninstall`, which removes the SDN zone and vnet last and
only once no guest of the cluster has a card on `pconet`; `pco uninstall --detach-managed`
removes the cards pco attached first and waits for the configurations to drop them. See
[Uninstall](../uninstall.md).

## Read on

- [Networking](../networking.md): consent, clones, DHCP, migration, what the host isolates
  and the limits.
- [The managed network of the appliance](managed-network-appliance.md): the two steps, the
  grants, the guest firewall and the level `filtered`.
- [Troubleshooting](../troubleshooting.md#networks-behind-a-leg-and-the-managed-network): a
  card that waits, no DHCP seen, a foreign card, the vnet changed.
- Reference: [pco network managed enable](../cli/pco-network-managed-enable.md),
  [pco managed status](../cli/pco-managed-status.md),
  [pco managed approve](../cli/pco-managed-approve.md),
  [pco managed revoke](../cli/pco-managed-revoke.md).
