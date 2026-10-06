# Publish a wildcard

This guide publishes a wildcard hostname, `*.shop.example.com`, that sends every name below it
to one reverse proxy, and admits it in the settings. The container in the examples is
`lxc/130`, named `shop`, at `10.0.0.30`, with a proxy on port 80.

## When you need it

One service answers for names that come and go, such as a shop with a name for each customer
or an application with a name for each branch, and it tells them apart by the Host header of
each request. A wildcard spares a route for each name.

A wildcard answers every name below it that has no rule of its own, with a valid certificate.
That is why pco publishes it only once an `allowHosts` pattern of the settings names it: a
guest whose Notes may be edited by others would otherwise catch every unused name of the zone.
The apex of a zone, `example.com` itself, is admitted the same way.

## Before you start

- The guest is published as in [Publish a container](publish-a-container.md) or
  [Publish a virtual machine](publish-a-vm.md), and the service behind it answers for every
  name it should, by the Host header.
- Mind the certificate. Cloudflare's universal certificate of a zone covers the zone and one
  level below it: `*.example.com` and `www.example.com`, not `a.shop.example.com`. A wildcard
  more than one level below the zone needs an advanced certificate at Cloudflare that covers
  it, which pco does not order; `pco routes` warns about such a route.
- You can save the settings: root on the node, or an admin in the web interface.

## Steps

1. Write the wildcard into the Notes of the guest, with the names it serves itself:

   ````text
   ```cf-tunnel
   shop.example.com *.shop.example.com -> :80
   ```
   ````

2. Ask for a cycle. The wildcard is `rejected` and names the pattern it needs; the rest of
   the Notes is published as usual:

   ```sh
   pco sync
   pco routes
   ```

   ```text
   HOSTNAME            STATE     LEVEL  SERVICE              OWNER    ZONE         NOTE
   *.shop.example.com  rejected  -      -                    lxc/130  example.com  a wildcard is published only when an allowHosts pattern names it: add "*.shop.example.com" to allowHosts
   shop.example.com    active    port   http://10.0.0.30:80  lxc/130  example.com  -
   ```

   `pco status` carries it as a problem, and `pco doctor` warns in its `rejected routes`
   check. A rejected route takes no claim, so naming a wildcard before it is admitted holds
   nothing for the guest.

3. Add the pattern to `allowHosts`. The list does two things at once: it names the wildcards
   and apexes that may be published, and, once it has any entry, it is the list of every
   name that may be published at all. Keep `*` in it to go on allowing every other hostname;
   `*` names no wildcard and no apex, so the wildcard needs its own entry:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq '.settings.allowHosts = ["*", "*.shop.example.com"]' /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

   ```text
   Saved the settings; they are at revision 5.
   ```

   `jq` is `apt install jq` away; an editor does the same. A wildcard pattern names the
   wildcards at and below it: `*.example.com` would admit `*.shop.example.com` too, and every
   other wildcard of the zone. Name the one you mean.

4. Ask for a cycle again. The wildcard is published, with the warning about its certificate:

   ```sh
   pco sync
   pco routes
   ```

## The order in which rules match

A tunnel sends a request to the first rule that matches its hostname. pco writes the rules in
an order that makes the most specific one win, whatever the order in the Notes: exact names
before wildcards, names with more labels first, and then in alphabetical order. So
`shop.example.com` and every name another route names exactly keep their own rule, and the
wildcard gets the rest.

When the wildcard of one guest covers a hostname of another guest, the exact name still goes
to its own guest, and the routes of both carry a warning in `NOTE`, as a reminder that a
mistake in one of them sends the name elsewhere:

```text
HOSTNAME              STATE   LEVEL  SERVICE                OWNER     ZONE         NOTE
*.shop.example.com    active  port   http://10.0.0.30:80    lxc/130   example.com  *.shop.example.com overlaps api.shop.example.com owned by qemu/101
api.shop.example.com  active  port   http://10.0.0.11:9000  qemu/101  example.com  api.shop.example.com overlaps *.shop.example.com owned by lxc/130
shop.example.com      active  port   http://10.0.0.30:80    lxc/130   example.com  -
```

`NOTE` shows the first warning; `pco routes --json` has every one, as `warnings`, where the
certificate warning, `more than one level below example.com: needs an advanced certificate`,
stands beside the overlap.

A name that is claimed and not served, as one of a stopped guest, has a rule that answers 503,
so that no wildcard serves it meanwhile.

`denyHosts` works the other way round. A deny pattern that names anything below a wildcard
denies the wildcard as a whole, because the wildcard would serve that name too:
`secret.shop.example.com` in `denyHosts` refuses `*.shop.example.com`.

## Check

`pco diagnose` takes the wildcard itself, in quotes for the shell; a name below it is no route
of its own:

```sh
pco diagnose '*.shop.example.com'
```

```text
✓ route      lxc/130 (shop) holds it; state active
✓ zone       zone example.com is active
✓ dns        its record points at the tunnel
✓ ingress    tunnel pco-7f3a9c0d41b2 sends it to http://10.0.0.30:80 (configuration version 4)
✓ connector  active, ready, 4 connections
✓ identity   10.0.0.30 is the address of lxc/130, verified at identity level port (static)
✓ tcp        10.0.0.30:80 answers
✓ http       the origin answered 200 OK
```

Its `http` step asks for a name below the wildcard, `pco-diagnose.shop.example.com`. From
outside, ask for a name of your own: `curl -sI https://anything.shop.example.com`. A TLS
error at that point, before any answer, is the certificate of the edge, not the service.

## Undo

Take the wildcard out of the Notes first and let its record go, after the grace period of a
minute. Then take the pattern out of `allowHosts`, the same way it went in. In the other order
the route is rejected at once, and a record pco made for it is kept and left alone, so that
the name serves again when a pattern names it.

## Read on

- [Annotations](../annotations.md#wildcards-and-their-order): wildcards in the grammar.
- [Settings](../settings.md#hostname-patterns): the patterns, and what names a wildcard.
- [Decide who may publish](who-may-publish.md): the policy as a limit on the Notes.
- Reference: [pco settings apply](../cli/pco-settings-apply.md), [pco routes](../cli/pco-routes.md).
