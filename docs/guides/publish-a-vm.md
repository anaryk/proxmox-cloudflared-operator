# Publish a virtual machine

This guide publishes the services of a virtual machine under several hostnames, on several
ports and on a second network card. The VM in the examples is `qemu/101`, named `web-1`, with
`10.0.0.11` on `vmbr0` and `10.0.1.11` on `vmbr1`.

## When you need it

A VM runs one or more services that should answer under hostnames of yours. What differs from
a container is how pco learns the address of the VM: Proxmox knows the address of a container,
and of a VM only when something tells it.

## Before you start

- What [Publish a container](publish-a-container.md#before-you-start) asks for holds here
  too: pco is set up with a token for the zone, the VM runs on the node that runs pco, the
  node has an address of its own in each network of the VM that a route uses, and the
  services listen on the VM's address.
- For a card with a VLAN tag, the node's address is on the interface of that VLAN, such as
  `vmbr0.20`. Read [the warning in Troubleshooting](../troubleshooting.md#a-route-is-unreachable)
  before you give the node an address in a network of guests.

## Steps

1. Let pco learn the address of the VM, in one of three ways. pco tries every address it
   learns and serves one only once it has proven it on the network; none of the three is
   trusted by itself.

   - **The QEMU guest agent**, for a VM that gets its address by DHCP or is configured inside.
     Enable it in the options of the VM, install it in the guest, and start the VM again
     from Proxmox, since a reboot from inside the guest does not add the device the agent
     talks through:

     ```sh
     qm set 101 --agent enabled=1
     qm reboot 101
     ```

     In a Debian or Ubuntu guest, `apt install qemu-guest-agent` installs it. From the node,
     see what it reports:

     ```sh
     qm guest cmd 101 network-get-interfaces
     ```

   - **A cloud-init address** in the Proxmox configuration of the VM, for a VM that takes its
     network from cloud-init. pco reads `ipconfig0` to `ipconfig31` as static addresses, and
     the VM takes a changed one when it starts again from Proxmox:

     ```sh
     qm set 101 --ipconfig0 ip=10.0.0.11/24,gw=10.0.0.1
     ```

   - **The address in the route**, `-> http://10.0.0.11:8080`, for a VM that has neither.
     The route then serves that address and no other.

2. Give the VM the gate tag. `--tags` replaces the tags it has, so name those too:

   ```sh
   qm set 101 --tags 'cf-tunnel'
   ```

3. Write the routes into the Notes of the VM, one hostname, or several, per entry:

   ````text
   ```cf-tunnel
   www.example.com   -> :8080
   api.example.com   -> :9000
   files.example.com -> :8080 via=net1
   ```
   ````

   `:8080` without an address means the address pco learned for the VM. `via=net1` takes the
   addresses of the second card, `net1`, and leaves those of `net0` out; without it, pco tries
   the static addresses of every card, then the reported ones, and serves the one proven at
   the highest level.
   `via=10.0.0.21` chooses one address when a card has several. Several hostnames in front of
   one arrow share the target:

   ````text
   ```cf-tunnel
   www.example.com blog.example.com -> :8080
   ```
   ````

4. Ask for a cycle and look at the routes:

   ```sh
   pco sync
   pco routes
   ```

   ```text
   HOSTNAME           STATE   LEVEL  SERVICE                OWNER     ZONE         NOTE
   api.example.com    active  port   http://10.0.0.11:9000  qemu/101  example.com  -
   files.example.com  active  port   http://10.0.1.11:8080  qemu/101  example.com  -
   www.example.com    active  port   http://10.0.0.11:8080  qemu/101  example.com  -
   ```

5. On a new install, which only observes, `pco plan` shows what would be done, and
   `pco apply` starts publishing; see [Publish a container](publish-a-container.md#steps).

## Check

```sh
pco diagnose www.example.com
```

```text
✓ route      qemu/101 (web-1) holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://10.0.0.11:8080 (configuration version 4)
✓ connector  active, ready, 4 connections
✓ identity   10.0.0.11 is the address of qemu/101, verified at identity level port (reported)
✓ tcp        10.0.0.11:8080 answers
✓ http       the origin answered 200 OK
```

The word in brackets at the end of `identity` says where the address came from: `reported` by
the guest agent, `static` from the Proxmox configuration, or `via` from the route.

When pco learns no address at all, the route waits and says so:

```text
HOSTNAME         STATE        LEVEL  SERVICE  OWNER     ZONE         NOTE
www.example.com  unreachable  -      -        qemu/101  example.com  no candidate address
```

and `pco diagnose` stops at `identity`:

```text
✓ route      qemu/101 (web-1) holds it; state unreachable: no candidate address
✓ zone       zone example.com is active
! dns        no record is published for it while its target is not served
! ingress    tunnel pco-7f3a9c0d41b2 answers 503 for it while its target is not served
✓ connector  active, ready, 4 connections
✗ identity   no candidate address
! tcp        skipped
! http       skipped
```

Run the agent, set the cloud-init address, or name the address in the route. When the agent
runs and the route still waits, `qm guest cmd 101 network-get-interfaces` shows what it
reports: an address counts only on the card whose MAC the agent gives with it. Every other
reason, such as `no ARP answer on vmbr0`, is in [Troubleshooting](../troubleshooting.md#a-route-is-unreachable).

## Undo

Take the entries out of the Notes, or the tag off the VM (`qm set 101 --delete tags` when it
has no other tag), and leave no mention of the hostnames behind. Each hostname answers 503 for
the grace period, a minute by default, and then its record is deleted. The guest agent and a
cloud-init address can stay; pco does not need them for anything else.

## Read on

- [Identity](../identity.md#candidates-and-their-order): which addresses are tried, and in
  which order.
- [Annotations](../annotations.md#options): `via=` and the other options.
- [Publish an HTTPS origin](https-origins.md) for a service that speaks TLS.
- Reference: [pco routes](../cli/pco-routes.md), [pco diagnose](../cli/pco-diagnose.md).
