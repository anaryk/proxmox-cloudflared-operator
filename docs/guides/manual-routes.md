# Publish a route by hand

This guide publishes hostnames with manual routes, which root makes on the node instead of the
Notes of a guest: to a machine that is no guest of the node, to a guest whose route should not
be up to its Notes, and to a service of the node itself.

## When you need it

- The service runs on a machine Proxmox VE does not know: a storage box, a physical server, a
  device in a network the node reaches. The examples use `10.0.5.20`, port 9000.
- A guest should publish a hostname, and the people who edit its Notes should not decide
  which.
- A service of the node itself should be reachable through the tunnel.

## What a manual route is

A manual route names a hostname and a target, and owns the hostname as `manual/<id>`, competing
in the claims like a guest. It is kept as a file in `/etc/pve/pco/routes/`, which the daemon
reads in every cycle. The hostname policy, `allowHosts` and `denyHosts`, does not apply to it,
and neither does admission mode `approve`: root made it.

Its target is one of two kinds:

- **An address.** Nothing proves that the address belongs to anything in particular, so the
  route's identity level is `manual`, and only the denylist, the rule that keeps the addresses
  of the nodes out, and a connection to the port apply. The address has to lie in a prefix of
  the setting `manualCIDRs`, which is empty until you fill it, so that no route can point at
  an address you did not name.
- **A guest.** Its address is proven as for a route in its Notes, at `port` or `observed`.
  `manualCIDRs` does not apply.

## Before you start

- You are root on the node; in the web interface, an admin.
- For a route to an address: the node reaches it, and you know the prefixes your manual routes
  may use. Keep them to the networks of the services you mean to publish.

## Steps

1. Name the prefixes manual routes may point into:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq '.settings.manualCIDRs = ["10.0.5.0/24"]' /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

   The prefixes are checked when a route is made or changed, so a change needs no restart.

2. Make the route. `--id` names it; without one the daemon gives it eight hex digits:

   ```sh
   pco route manual add status.example.com --address 10.0.5.20 --port 9000 --id status
   ```

   ```text
   Made manual route manual/status: status.example.com -> http://10.0.5.20:9000.
   It is published from the next cycle; pco routes shows how it fares.
   ```

   The options are those of the Notes: `--https`, `--no-tls-verify`, `--sni <name>` and
   `--host-header <name>`; [Publish an HTTPS origin](https-origins.md) says when each is
   needed. An address outside `manualCIDRs` is refused:

   ```text
   pco: target.addr 192.0.2.10: not inside the manualCIDRs of the settings (10.0.5.0/24)
   ```

3. For a route to a guest, name the guest instead of the address. `--via` chooses a card or an
   address of it, as `via=` does in the Notes:

   ```sh
   pco route manual add wiki.example.com --guest qemu/101 --port 8443 --https --no-tls-verify --via net1
   ```

   When the Notes of that guest name the same hostname, whichever claimed it first holds it;
   [Move a hostname to another guest](move-a-hostname.md) hands it over, with `manual/<id>` as
   the owner.

4. For a service of the node itself, add `--allow-node`. The prefix of the node's address has
   to be in `manualCIDRs` as well, and the route is refused without the option:

   ```text
   pco: target.addr 10.0.0.2: an address of a node; a route to a service of the node needs allowNode
   ```

   ```sh
   pco route manual add node-status.example.com --address 10.0.0.2 --port 8080 --allow-node --id node-status
   ```

   `--allow-node` lifts the rules that keep the addresses of the nodes out, and nothing else,
   and the egress filter lets the connectors reach that address and port. Anything you publish
   is public, and pco puts no sign-in in front of it: never publish the web interface of
   Proxmox VE, port 8006, or SSH this way.

## Check

```sh
pco route manual list
```

```text
ID      REV  HOSTNAME            TARGET                 OPTIONS
status  1    status.example.com  http://10.0.5.20:9000  -
```

```sh
pco routes
```

```text
HOSTNAME            STATE   LEVEL   SERVICE                OWNER          ZONE         NOTE
app.example.com     active  port    http://10.0.0.12:3000  lxc/120        example.com  -
status.example.com  active  manual  http://10.0.5.20:9000  manual/status  example.com  -
```

```sh
pco diagnose status.example.com
```

```text
✓ route      manual/status holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://10.0.5.20:9000 (configuration version 4)
✓ connector  active, ready, 4 connections
✓ identity   10.0.5.20 is the address of manual/status, verified at identity level manual (via)
✓ tcp        10.0.5.20:9000 answers
✓ http       the origin answered 200 OK
```

`pco egress show` lists a target of a route with `--allow-node` with `(allowNode)` after it.

## Change and undo

There is no command that changes a manual route: remove it and make it again, or change it in
the web interface, which keeps its id. Removing shows the route and asks:

```sh
pco route manual remove status
```

```text
manual/status publishes status.example.com -> http://10.0.5.20:9000 (revision 1).
Removing it stops publishing status.example.com from the next cycle.
Remove manual route manual/status? [y/N] y
Removed manual route manual/status.
```

The hostname then goes as one taken out of the Notes does: it answers 503 for the grace period
and its record is deleted after it, unless another owner claims it.

Do not write the files in `/etc/pve/pco/routes/` by hand. A file root writes there is not
checked against `manualCIDRs`.

## Read on

- [Security](../security.md#manual-routes): what a manual route is trusted with.
- [Identity](../identity.md#the-denylist-and-the-node): the addresses that are never published,
  and what `allowNode` lifts.
- Reference: [pco route manual add](../cli/pco-route-manual-add.md),
  [pco route manual list](../cli/pco-route-manual-list.md),
  [pco route manual remove](../cli/pco-route-manual-remove.md).
