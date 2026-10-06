# Take over existing DNS records

This guide moves hostnames that already resolve, through a `cloudflared` you run yourself or
through an address record, to pco, one hostname at a time and without a gap. The hostname in
the examples is `shop.example.com`, served today by a tunnel of your own, and from now on by
the container `lxc/130` (`shop`) at `10.0.0.30`.

## When you need it

- You run `cloudflared` by hand or as a service, on the node or in a guest, with a tunnel you
  made in the dashboard or with `cloudflared tunnel create`, and its hostnames have CNAME
  records that point at it.
- A hostname has an A or AAAA record that points at a public address, with a port forwarded
  to the guest.

pco never changes or deletes a DNS record that does not carry its marker, a comment that
begins with `pco:` and the install id. Such a record is in the way of a route for the same
name: the route waits, and the record keeps serving as before, until you tell pco to take it
over with `pco adopt`.

## Before you start

- pco is set up and has a token that serves the zone ([Quickstart](../quickstart.md)).
- Each hostname to move is held by one address record, an A, AAAA or CNAME record. A name that
  has an A and an AAAA record is held by two, and pco does not choose between them: delete one
  at Cloudflare first. TXT and other records at the name stay where they are.
- The guest that is to serve the name runs the service. Your old setup goes on serving until
  the moment the record changes, so both can run side by side.

## Steps

1. Tag the guest and write the route for the hostname into its Notes, as in
   [Publish a container](publish-a-container.md):

   ````text
   ```cf-tunnel
   shop.example.com -> :80
   ```
   ````

   `pco routes` shows the route `active` once its address is verified. That is the route; the
   record is the next question.

2. On a new install, which only observes, run `pco apply`. Records in the way show only once
   pco has its tunnel, which `pco apply` creates: before that, `pco plan` lists a
   `create-record` for the name, held by `tunnel not created yet`, whether a record of someone
   else holds it or not. `pco apply` changes nothing for a name that a record of someone else
   holds; it creates the records of the names that are free.

3. After the next cycle, `pco plan` lists the record in the way:

   ```sh
   pco plan
   ```

   ```text
   Records of someone else that stand in the way (pco adopt replaces one):
   NAME              ZONE         TYPE   CONTENT
   shop.example.com  example.com  CNAME  4d3c2b1a-0f9e-4d8c-b7a6-958473625140.cfargotunnel.com
   ```

   `pco diagnose shop.example.com` stops at the same place, and says what to do:

   ```text
   ✓ route      lxc/130 (shop) holds it; state active
   ✓ zone       zone example.com is active
   ✗ dns        a record of someone else holds the name in zone example.com: CNAME 4d3c2b1a-0f9e-4d8c-b7a6-958473625140.cfargotunnel.com; pco adopt shop.example.com replaces it
   ! ingress    skipped
   ! connector  skipped
   ! identity   skipped
   ! tcp        skipped
   ! http       skipped
   ```

4. Take the record over:

   ```sh
   pco adopt shop.example.com
   ```

   ```text
   shop.example.com is held by a record of someone else in zone example.com: CNAME 4d3c2b1a-0f9e-4d8c-b7a6-958473625140.cfargotunnel.com.
   Adopting replaces it with a record that points at the tunnel.
   Adopt shop.example.com? [y/N] y
   Adoption of shop.example.com requested; it is made by the next run that can.
   ```

   The next run that can, in which the tunnel of pco is verified and its connector is ready,
   first copies the record as it is into `/etc/pve/pco/adopted.jsonl` and then replaces it.
   A CNAME is changed in place, so the name moves from one tunnel to the other in one step. An
   A or AAAA record is deleted and a CNAME created in its place; if the create fails, the old
   record is put back. A request that no run could make in five minutes expires, with an
   event.

5. Repeat steps 3 and 4 for every hostname, at the pace you like. Each one is a step of its
   own, and the others go on as they were.

6. When no hostname is left on it, stop your old connector. If it ran on the node as the
   service `cloudflared service install` makes:

   ```sh
   systemctl disable --now cloudflared.service
   ```

   Then delete the old tunnel at Cloudflare, once no record points at it. pco never deletes a
   tunnel it did not make.

## Check

```sh
pco events --since 10m
pco diagnose shop.example.com
```

The events have the request, `adoption requested for the next run`, and its outcome,
`adoption of shop.example.com applied: the record was replaced`, beside the change itself, an
`action` with the record as it was:

```text
update-record in zone example.com: CNAME 0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d.cfargotunnel.com, was CNAME 4d3c2b1a-0f9e-4d8c-b7a6-958473625140.cfargotunnel.com
```

`pco plan` no longer lists the name, and every step of `pco diagnose` passes.

A connector of yours that runs on the node is not a connector of pco: the egress filter does
not confine it, and pco's connectors never read its configuration in `/etc/cloudflared`. Until
you stop it, it serves whatever its tunnel still routes.

## Undo

The record as it was is kept, one JSON line for each adoption:

```sh
jq -c '{at, zone, record: (.record | {type, name, content, proxied})}' /etc/pve/pco/adopted.jsonl
```

To give a hostname back to your old setup, start your old connector again if you stopped it.
Then edit the record at Cloudflare back to the old content and clear its comment, which begins
with `pco:` and the install id: without the marker, the record is no longer pco's, and pco
leaves it alone. Last, take the hostname out of the Notes of the guest.

Copy `adopted.jsonl` before `pco uninstall`, which removes it with the store: a purge deletes
pco's records, and the records they replaced do not come back by themselves.

## Read on

- [Troubleshooting](../troubleshooting.md#records-in-the-way-and-pco-adopt): records in the
  way, and records that lost their marker, which `pco adopt` takes back too.
- [Cloudflare token](../cloudflare-token.md#what-pco-names-and-marks): what pco names and
  marks at Cloudflare.
- Reference: [pco adopt](../cli/pco-adopt.md), [pco plan](../cli/pco-plan.md).
