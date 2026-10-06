# Publish a container

This guide publishes a service that runs in a Proxmox VE container under a hostname of your
zone, `app.example.com`, and checks that it answers. The container in the examples is
`lxc/120`, named `app-1`, at `10.0.0.12`, and its service listens on port 3000.

## When you need it

A service in a container should answer on the internet under a hostname of yours, and you do
not want to open a port of your network for it. The same steps hold for every further
container; [Publish a virtual machine](publish-a-vm.md) has what differs for a VM.

## Before you start

- pco is set up on the node and holds a Cloudflare token that serves the zone of the
  hostname; [Quickstart](../quickstart.md) takes a node that far.
- The container runs on the node that runs pco. A container of another node is proven at a
  lower level and is not served by default; see [Run pco on a cluster](clusters.md).
- The container has an IPv4 address on a bridge, or a VLAN of one, where the node has an
  address of its own. On a default install that is `vmbr0`. [Identity](../identity.md#what-is-checked)
  says why the node has to be in the guest's network.
- The service listens on that address, not only on 127.0.0.1.
- You work as root on the node, or as a Proxmox user with `Sys.Modify` on `/`, which the gate
  tag needs once it is registered, and `VM.Config.Options` on the container for its Notes.

## Steps

1. Look at the network card of the container. pco takes the address from its `ip=`, or, while
   the container runs, from what Proxmox reads from it, so an address from DHCP works too.

   ```sh
   pct config 120 | grep '^net'
   ```

   ```text
   net0: name=eth0,bridge=vmbr0,gw=10.0.0.1,hwaddr=BC:24:11:5E:7A:12,ip=10.0.0.12/24,type=veth
   ```

2. Give the container the gate tag, `cf-tunnel` unless the setting `gateTag` names another.
   In the Proxmox VE web interface, select the container, click the pencil beside its tags
   in the header, add `cf-tunnel` and confirm. On the command line:

   ```sh
   pct set 120 --tags 'cf-tunnel'
   ```

   `--tags` replaces the tags the container has. Name those too, separated by semicolons, as
   `--tags 'web;cf-tunnel'`.

3. Write the route into the Notes of the container. In the Proxmox VE web interface, open its
   Summary, click Edit on the Notes panel, add the block and save:

   ````text
   ```cf-tunnel
   app.example.com -> :3000
   ```
   ````

   The text around the block stays as it is; pco takes its routes from the block alone. On
   the command line the one-line form is the easier one, but `--description` replaces the
   whole Notes:

   ```sh
   pct set 120 --description 'cf-tunnel: app.example.com -> :3000'
   ```

   [Annotations](../annotations.md) has the grammar: more hostnames, other ports, `https`, and
   the options.

4. Ask the daemon for a cycle, rather than wait for the next one, which comes within ten
   seconds:

   ```sh
   pco sync
   ```

5. See how the route fares:

   ```sh
   pco routes
   ```

   ```text
   HOSTNAME         STATE   LEVEL  SERVICE                OWNER    ZONE         NOTE
   app.example.com  active  port   http://10.0.0.12:3000  lxc/120  example.com  -
   ```

   `active` at level `port` means that pco found the address, proved that it is the
   container's, and reached the port. A route that is not `active` says why in `NOTE`;
   [Troubleshooting](../troubleshooting.md#a-route-is-unreachable) has every reason. A
   mistake in the Notes is listed under `Issues` in `pco status`, with its line and column.

6. See what the daemon does with it:

   ```sh
   pco plan
   ```

   On an install that publishes already, the cycle has made the record and the rule by now,
   and the plan says `Nothing to do.` On a new install, which only observes, it lists what
   `pco apply` would do:

   ```text
   ACTION         TARGET            DETAIL                                                                     HELD
   create-tunnel  pco-7f3a9c0d41b2  in account 0123456789abcdef0123456789abcdef                                observe mode
   put-config     pco-7f3a9c0d41b2  in account 0123456789abcdef0123456789abcdef: 3 rules, first configuration  observe mode
   create-record  app.example.com   in zone example.com: tunnel pco-7f3a9c0d41b2 has no known id               tunnel not created yet
   ```

7. On a new install, start publishing when the plan is what you expect:

   ```sh
   pco apply
   ```

   ```text
   Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status.
   ```

   [Roll out in observe-only mode](observe-then-enforce.md) says more about that step. On an
   install that publishes already there is nothing to do: the next cycle publishes the route.

## Check

`pco diagnose` follows the hostname from its route to the service and stops at the first
step that fails:

```sh
pco diagnose app.example.com
```

```text
✓ route      lxc/120 (app-1) holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://10.0.0.12:3000 (configuration version 4)
✓ connector  active, ready, 4 connections
✓ identity   10.0.0.12 is the address of lxc/120, verified at identity level port (static)
✓ tcp        10.0.0.12:3000 answers
✓ http       the origin answered 200 OK
```

The events show what was done at Cloudflare:

```sh
pco events --since 10m
```

```text
TIME                       LEVEL  KIND     SUBJECT           MESSAGE
2026-10-01T13:58:00+02:00  info   route    app.example.com   lxc/120: active
2026-10-01T13:58:00+02:00  info   action   pco-7f3a9c0d41b2  put-config in account 0123456789abcdef0123456789abcdef: 6 rules replace version 3
2026-10-01T13:58:00+02:00  info   action   app.example.com   create-record in zone example.com: CNAME 0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d.cfargotunnel.com
2026-10-01T13:58:10+02:00  info   rollout  pco-7f3a9c0d41b2  configuration version 4 runs on 1 connector in account 0123456789abcdef0123456789abcdef
```

Last, ask from a machine outside your network. The answer is the service's own:

```sh
curl -sI https://app.example.com
```

An empty 503 or 404, a 502 or Cloudflare's error 1033 are the tunnel's answers, not the
service's; [Troubleshooting](../troubleshooting.md#what-a-visitor-sees) says what each means.

## Undo

Take the entry out of the Notes, or take the tag off the container (`pct set 120 --delete tags`
when it has no other tag). Leave no mention of the hostname behind in the Notes: pco takes
any word of the Notes that is a hostname as a sign that the container still asks for it.

The hostname is then held for the grace period, a minute by default, and answers 503
meanwhile, so that a short edit of the Notes loses nothing:

```text
HOSTNAME         STATE  LEVEL  SERVICE  OWNER    ZONE         NOTE
app.example.com  held   -      -        lxc/120  example.com  no longer claimed by lxc/120 since 2026-10-01T12:05:00Z; released after the grace period
```

After it, the claim is released and the DNS record is deleted (`delete-record` in
`pco events`). The name no longer resolves.

## Read on

- [Annotations](../annotations.md): everything the Notes can say.
- [Identity](../identity.md): how pco proves that an address is the container's.
- [Publish an HTTPS origin](https-origins.md), [Publish a wildcard](wildcards.md).
- Reference: [pco routes](../cli/pco-routes.md), [pco diagnose](../cli/pco-diagnose.md),
  [pco plan](../cli/pco-plan.md), [pco apply](../cli/pco-apply.md).
