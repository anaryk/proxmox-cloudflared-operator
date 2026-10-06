# Work with the egress filter

This guide reads the egress filter that confines the connectors, recognises a connection it
rejects, keeps the connectors from one address at once, and switches the filter off and on
when it is itself the fault.

## When you need it

- Visitors get a 502 for a hostname that `pco diagnose` passes at every step.
- You want to see what the connectors on the node may reach.
- A guest misbehaves and its address should be out of reach of the connectors now, before
  anything else is decided.
- `pco status` or `pco doctor` says the filter is off, not loaded, or not as pco loads it.

## What the filter allows

The filter is the nftables table `inet pco_egress`. It looks only at the packets of the user
the connectors run as, `pco-connector`, and lets them reach:

- the targets the daemon verified, by address and port, and the targets of manual routes with
  `allowNode`;
- the resolvers of the node, the `nameserver` lines of `/etc/resolv.conf`, on port 53;
- Cloudflare's edge, port 7844 of a public address, and DNS over TLS to 1.1.1.1 and 1.0.0.1.

Everything else is rejected: TCP with a reset and the rest with an ICMP message, so that a
connection fails at once instead of waiting. Whoever rewrites the tunnel's configuration at
Cloudflare, with a stolen token or in the dashboard, can therefore not point a connector at the
web interface of the node or at any other host. [Security](../security.md#the-connector-egress-filter)
has the rules in their order.

The daemon gives the filter its targets in every cycle, before it writes anything at Cloudflare,
and takes an address out at once when it loses its proof or its MAC moves. Its own requests,
those of `pco diagnose` among them, are not the connector's and pass the filter by.

## Before you start

- You are root on the node. The `pco egress` commands work on the node directly, without the
  daemon.

## Steps

1. See the filter:

   ```sh
   pco egress show
   ```

   ```text
   The egress filter is on.

   Targets:
     10.0.0.11:8080
     10.0.0.11:9000
     10.0.0.12:3000
     10.0.1.11:8080
   Resolvers:
     10.0.0.1

   Rejected since the table was last loaded in full:
     to addresses of this node  3 packets
     to anything else           12 packets

   Blocked on this node: none
   ```

   Every target of an `active` route should be in `Targets`. Its exit status is 1 when the
   filter is off, not loaded, or not as pco loads it, so a script can ask
   `pco egress show > /dev/null`.

2. Recognise a rejection. A connector that tries a target the filter does not hold gets a
   refused connection, and the visitor a 502; the counter `to anything else` rises, or `to
   addresses of this node` for an address of the node. The connector's journal has the
   failed request to the origin:

   ```sh
   journalctl -u pco-cloudflared@0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d --since '10 min ago'
   ```

   A target that is missing is usually one of these:

   - The node or the daemon has just started: the targets come with the first cycle that has
     verified them, and until then every hostname gets a 502.
   - The MAC of the address moved, and the address was taken out until it is verified again.
     `pco events` has it under the kind `egress`: `the MAC of 10.0.0.11 moved; the connectors
     do not reach it until it is verified again`, and then either `10.0.0.11 was verified again
     after its MAC moved; the connectors reach it again` or the reason it failed.
   - The route is not `active`: `pco routes` and the `identity` step of `pco diagnose` say why.
   - The address is on the block list of this node, below.

3. Keep the connectors from an address at once, whatever the daemon verified:

   ```sh
   pco egress block 10.0.0.12
   ```

   ```text
   Blocked 10.0.0.12.
   Took 1 entry for it out of the egress table, which now rejects it on every port.
   ```

   The block is kept in `/var/lib/pco/egress-blocked.json`, for this node only, and taken out
   of every set the daemon loads until you lift it. It changes nothing at Cloudflare: the
   hostnames of that address stay published and answer 502. Take them out of the Notes, or
   the tag off the guest, to unpublish them.

4. Only when the filter is itself the fault, switch it off, try again, and switch it on at
   once. While it is off the connectors can reach everything the node can:

   ```sh
   pco egress off
   ```

   ```text
   Switched the egress filter off: the connectors are not confined until pco egress on.
   ```

   ```sh
   pco egress on
   ```

   ```text
   Switched the egress filter on and loaded its table: the connectors reach the resolvers of the node and Cloudflare's edge, and nothing else until the daemon adds their verified targets at its next cycle.
   ```

   While it is off, `pco status` starts with a warning and exits 1, `pco doctor` fails its
   `egress` check, and both are events. If a route works with the filter off and not with it
   on, keep the output of `pco egress show` and the journal: the filter refuses something it
   should allow.

5. When the table is gone or changed and the daemon cannot load it again, load it by hand:

   ```sh
   pco egress load
   ```

   Do not restart `pco-egress.service` for that: every connector requires it, and restarts
   with it. `nft flush ruleset`, and an enabled `nftables.service` that starts or reloads,
   remove the table; `pco doctor` warns in its `nftables` check while that unit is enabled.

## Check

```sh
pco egress show
pco status
pco doctor
```

`pco egress show` says `The egress filter is on.` and exits 0, `pco status` says `Egress: on`,
and the `egress` check of `pco doctor` says `the egress filter confines the connectors`.

## Undo

Lift a block. The daemon puts the address back at its next cycle if it is still a verified
target:

```sh
pco egress unblock 10.0.0.12
```

```text
Unblocked 10.0.0.12: the daemon puts it back into the egress table at its next cycle if it is still a verified target.
```

Switch a filter that is off back on with `pco egress on`, as above.

## Read on

- [Security](../security.md#the-connector-egress-filter): the rules, how the daemon keeps the
  targets, the table at boot, and what the filter does not stop.
- [Troubleshooting](../troubleshooting.md#a-connector-cannot-reach-its-target): a connector
  that cannot reach its target.
- Reference: [pco egress show](../cli/pco-egress-show.md),
  [pco egress block](../cli/pco-egress-block.md), [pco egress off](../cli/pco-egress-off.md),
  [pco egress on](../cli/pco-egress-on.md).
