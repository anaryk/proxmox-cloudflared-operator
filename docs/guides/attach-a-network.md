# Attach a network

This guide reaches the guests of a bridge the node has no address in, by attaching the
network: a leg of pco's own goes into the bridge, and the guests there are served through
it, untouched. The bridge in the examples is `vmbr1`, a bridge without a physical port that
holds the lab network `10.77.0.0/24`, with the container `lxc/130`, named `lab-1`, at
`10.77.0.11`, and its service on port 80.

## When you need it

A guest sits on a bridge, or a VLAN of one, where the node has no address, and you do not
want to give the node one there: the management of the node would then be in reach of every
guest of that network, and the network may not be yours to change. `pco routes` shows its
route `unreachable` with `node has no address on vmbr1 in the guest's network`.
[Networking](../networking.md#which-way-to-a-guest) says when to attach and when the managed
network is the better answer.

## Before you start

- pco is set up on the node and publishes already, or is ready to;
  [Quickstart](../quickstart.md) takes a node that far.
- You know the prefix of the guests' network and one address in it that nothing holds and
  that no DHCP server there gives out. The leg takes it for as long as the network is
  attached.
- On a bridge with a physical port, a LAN with other stations on it, the leg's MAC and
  address are seen by them. Reserve the address with whoever runs that LAN.
- In the appliance, the container has the grant of the bridge, made on the node with
  `install.sh --appliance --grant-network --vmid 120 --bridge vmbr1`
  ([Commands on the node](../appliance.md#commands-on-the-node)); with the `pco` of the
  release, [`pco appliance network allow vmbr1 --vmid 120`](../cli/pco-appliance-network-allow.md)
  is the same in short. Every `pco` command of the steps below then runs inside the container,
  as `pct exec 120 -- pco ...`. A VLAN is attached on a VLAN-aware bridge only.
- You work as root on the node.

## Steps

1. Look at what the node could attach:

   ```sh
   pco network candidates
   ```

   ```text
   SEGMENT   KIND                UPLINK  GUESTS  PREFIX        STATE
   vmbr0     bridge              yes     0       -             direct: the node has an address here
   vmbr1     bridge              no      1       10.77.0.0/24  can be attached
   vmbr2:20  bridge, VLAN-aware  yes     0       -             can be attached
   ```

   `vmbr0` is direct: the node reaches its guests as itself, and there is nothing to attach.
   `vmbr1` holds one guest whose address is known, and the prefix those addresses suggest.
   `UPLINK` says whether a physical port or a bond is enslaved to the bridge. With `--json`
   the answer lists every guest address by guest.

2. Ask who answers for the address the leg would take. Without `--address` pco proposes the
   highest address of the prefix that no guest is known to have:

   ```sh
   pco network probe vmbr1 --address 10.77.0.250/24
   ```

   ```text
   10.77.0.250 on vmbr1, in 10.77.0.0/24, is free: nothing answered the probe.
   ```

   An address in use says who holds it, `is in use: 52:54:00:12:34:56 answered the probe`,
   and the command exits 1. The probe is an ARP probe from no address, through a port of
   pco's own on the bridge while the network is not attached; on a bridge with a physical
   port its MAC is seen on that LAN for the moment it probes, and the output says so first.

   The appliance has no port of a bridge before its card is there, so it cannot probe the
   address yet. It says so, and the command exits 1:

   ```text
   10.77.0.250 on vmbr1, in 10.77.0.0/24, is not probed: the appliance has no network card on vmbr1 before the network is attached; the leg checks 10.77.0.250 when it is raised and keeps it off if anyone answers.
   ```

3. Attach the network. The command probes the address again, shows the result and what it
   found of the bridge, and asks:

   ```sh
   pco network attach vmbr1 --address 10.77.0.250/24
   ```

   ```text
   10.77.0.250 on vmbr1, in 10.77.0.0/24, is free: nothing answered the probe.
   Attach network vmbr1 with the leg 10.77.0.250/24? [y/N] y
   Attached network vmbr1: the leg leg1 with 10.77.0.250/24, mark 1. The next cycle raises it; pco network list shows how it is.
   ```

   Two things make the command refuse, each with what to pass when you mean it:

   - A bridge with a physical uplink needs `--confirm-uplink`: `vmbr2:20 has a physical
     uplink: the leg's MAC becomes visible on the LAN of vmbr2`. The address has to be one
     reserved for pco there.
   - A prefix that overlaps a route of the node on another interface needs `--overlap-ok`:
     `192.168.4.0/24 overlaps the node's route on vmbr0; pass --overlap-ok if the guests
     there are meant to be reached through the leg`. That is right for a bridge that carries
     a copy of the LAN's addresses, as a lab does; the node's own traffic stays where it is.

   A script passes `--yes`. The first network of a host enables `pco-netns.service`, the unit
   that brings the gateway namespace back at boot. In the appliance the attach shows the
   `is not probed` line above in place of the probe's result and asks all the same; the leg
   checks the address when it is raised, as step 4 shows.

4. Wait for the next cycle, and look at the network:

   ```sh
   pco network list
   ```

   ```text
   ID     KIND    BRIDGE  VLAN  PREFIX        LEG          STATE  ROUTES
   vmbr1  attach  vmbr1   -     10.77.0.0/24  10.77.0.250  up     1
   ```

   `STATE` is how the gateway last found the leg: `up`, or why not, as `address in use by
   52:54:00:12:34:56` when another station took the address meanwhile (in the appliance, also
   when it held it before the attach, which could not probe), or the bridge being absent. A leg that is not up is a problem line of `pco status` and a failed check `leg
   vmbr1` of `pco doctor`.

5. Publish a guest of the network as any other: the gate tag and the Notes, as in
   [Publish a container](publish-a-container.md). Nothing in the Notes names the network;
   pco finds that the guest's card is on `vmbr1` and proves its address through the leg,
   with ARP from the leg and the forwarding table of the bridge, like a guest the node is on.

   ```sh
   pco routes
   ```

   ```text
   HOSTNAME         STATE   LEVEL  SERVICE               VIA           OWNER    ZONE         NOTE
   lab.example.com  active  port   http://198.18.1.0:80  attach:vmbr1  lxc/130  example.com  -
   ```

   `VIA` says that the route is reached through the leg of `vmbr1`, and `SERVICE` is the
   service address the connectors dial for the guest: an address of `198.18.0.0/16` that the
   gateway translates to `10.77.0.11` and sends out of the leg. The egress filter holds the
   service address and never the real one. Right after an attach, or a restart of the daemon,
   a route can show `unreachable` with `service address not allocated: no cycle has run since
   the start` for one cycle: a new service address is made only once the daemon knows it is
   the writer, and the next cycle makes it.

## Check

`pco diagnose` walks the chain, and its `path` step says how the node reaches the guest:

```sh
pco diagnose lab.example.com
```

```text
✓ route      lxc/130 (lab-1) holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://198.18.1.0:80 (configuration version 5)
✓ connector  active, ready, 4 connections
✓ identity   10.77.0.11 is the address of lxc/130, verified at identity level port (static)
✓ path       attach vmbr1: leg up, service 198.18.1.0 -> 10.77.0.11 programmed, 10.77.0.11 pinned to bc:24:11:00:01:30
✓ tcp        198.18.1.0:80 answers
✓ http       the origin answered 200 OK
```

The `tcp` and `http` steps ask the service address from the daemon, which is the exact way of
the connector. `pco network show vmbr1` lists the leg with its mark and MAC, the service
addresses and the routes served through it; on the node, `pco netns status` compares the
namespace with what the daemon applied and exits 1 when anything differs, and
`ip netns exec pco ip -br addr` shows the leg with its address. From a guest of the network,
`10.77.0.250` answers ARP from `02:50:43:4f:00:01` and nothing else: the namespace accepts
nothing from a leg.

A reboot of the node brings the leg back before the guests start: `pco-netns.service` brings
the namespace back from the cache the daemon writes after every change, ordered before
`pve-guests.service`. `systemctl restart pco-netns` is safe at any time; it brings back what is
missing and stops nothing.

## Undo

```sh
pco network detach vmbr1
```

```text
Network vmbr1: 10.77.0.0/24 on vmbr1, the leg at 10.77.0.250.
1 route is served through it:
  lab.example.com
Detaching it removes the leg and releases 1 service address; the routes through it are no longer served that way.
Detach network vmbr1? [y/N] y
Detached network vmbr1.
```

The leg goes at once and its service addresses are released; the routes of the guests there
are `unreachable` from the next cycle, with `node has no address on vmbr1 in the guest's
network`, until the network is attached again or the guests are reached another way. The last
network detached disables `pco-netns.service`; the namespace itself stays, with nothing in it,
until `pco uninstall` removes it.

In the appliance the grant of the bridge stays after a detach. Take it back on the node with
`install.sh --appliance --revoke-network --vmid 120 --bridge vmbr1`, the installer's way to
run [pco appliance revoke-network](../cli/pco-appliance-revoke-network.md)
([Commands on the node](../appliance.md#commands-on-the-node)).

## Read on

- [Networking](../networking.md): the gateway, the legs and the service addresses, and what
  the namespace keeps a guest from.
- [Set up the managed network](managed-network.md): when the guests should get a network of
  pco's own instead.
- [Troubleshooting](../troubleshooting.md#networks-behind-a-leg-and-the-managed-network): a leg
  that is not up, an address in use, a bridge that is absent.
- Reference: [pco network candidates](../cli/pco-network-candidates.md),
  [pco network probe](../cli/pco-network-probe.md),
  [pco network attach](../cli/pco-network-attach.md),
  [pco network list](../cli/pco-network-list.md), [pco network show](../cli/pco-network-show.md),
  [pco network detach](../cli/pco-network-detach.md),
  [pco netns status](../cli/pco-netns-status.md).
