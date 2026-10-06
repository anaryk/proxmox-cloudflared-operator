# Rotate the tunnel token

This guide gives the tunnel of the install a new secret, so that its old run token stops
working: a connector that someone runs elsewhere with that token is cut off, and the connector
of pco carries on with the new one.

## When you need it

- `pco status` names a connector on the tunnel that pco does not run on this node, and you do
  not run it either.
- A Cloudflare API token of pco may have leaked. A token with `Cloudflare Tunnel: Edit`, which
  pco's own needs, reads the run token of the tunnel with one request.
- A copy of `/var/lib/pco/tunnels`, where the run tokens are, may have left the node, or
  someone who had root on the node no longer should.

The run token is what a connector opens the tunnel with, and it is not the API token you gave
pco. Whoever holds it can run a connector of the tunnel anywhere, and Cloudflare then spreads
the requests to every hostname of the tunnel over all its connectors: that connector gets a
share of the traffic, in the clear, and answers it as it likes. pco reports such a connector
and does not cut it off by itself. [Security](../security.md#a-connector-that-is-not-pcos)
says why.

## Before you start

- You are root on the node; the daemon answers this request for root alone.
- pco publishes: the rotation is refused in observe-only mode, which changes nothing at
  Cloudflare, with `pco: refused: pco is in observe-only mode and changes nothing at
  Cloudflare; run pco apply to end it`.
- If the API token may have leaked, replace it first, or the rotation buys nothing: whoever
  holds it reads the new run token as easily as the old one. Add a new token with
  `pco credential add`, revoke the old one at Cloudflare, and `pco credential remove` it, as
  [Cloudflare token](../cloudflare-token.md#rotating-a-token) describes.

## Steps

1. Read what pco found. `pco status` exits 1 and has a line for each connector it does not
   run:

   ```text
   Problems:
     - tunnel pco-7f3a9c0d41b2 in account 0123456789abcdef0123456789abcdef is served by connector 0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 from 198.51.100.7 (cloudflared 2026.8.0), which pco does not run on this node: it takes a share of the requests to every hostname of the tunnel; unless you run it, rotate the tunnel secret with pco tunnel rotate --account 0123456789abcdef0123456789abcdef
   ```

   The address and the version are what Cloudflare reports of it. `pco events` has an
   `error` event of the kind `connector` from the first time it was seen, and `pco doctor`
   fails its `rogue connectors` check.

2. Rotate. The command says what it does, lists the connectors it cuts off, and asks; with
   tunnels in several accounts, name the account with `--account`:

   ```sh
   pco tunnel rotate --account 0123456789abcdef0123456789abcdef
   ```

   ```text
   This gives tunnel pco-7f3a9c0d41b2 (0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d) in account 0123456789abcdef0123456789abcdef a new secret at Cloudflare and ends
   the connections of every connector of it. The connector pco runs on this node restarts with
   the new token; any other connector loses its session and cannot connect again.
   Cloudflare lists connectors on it that pco does not run on this node:
     - 0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 from 198.51.100.7 (cloudflared 2026.8.0)
   Rotate the secret of tunnel pco-7f3a9c0d41b2? [y/N] y
   The secret of tunnel pco-7f3a9c0d41b2 in account 0123456789abcdef0123456789abcdef was rotated; its connector on this node restarts with the new token.
   Follow it with pco status.
   ```

   It works while the cycle holds, as long as the tunnel and its credential are known. A
   tunnel left as it is for a reason of its own, as in a frozen account, is refused.

## What visitors notice

Every hostname of the tunnel is unreachable for the few seconds the connector on the node takes
to reconnect with the new token: visitors get Cloudflare's error 1033 meanwhile. The DNS records
and the configuration of the tunnel do not change. For a rotation that does not press, choose a
quiet hour.

## Check

```sh
pco status
pco events --since 10m
```

The connector of the tunnel is `active, ready` again. The problem line goes once Cloudflare no
longer lists the other connector; pco lists the connectors every 30 seconds while such a
finding is pending. The events have the rotation, as an `admin` event, `the secret of tunnel
pco-7f3a9c0d41b2 in account 0123456789abcdef0123456789abcdef was rotated: every connector of
the tunnel was disconnected, and the one on this node restarts with the new token`, and, of the
kind `connector`, `connector 0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 no longer serves tunnel
pco-7f3a9c0d41b2 in account 0123456789abcdef0123456789abcdef`.

If the other connector comes back, its holder has a way to the new token: the API token, or root
on the node. Replace the API token, and look at who can read `/var/lib/pco/tunnels`.

## Undo

A rotation cannot be taken back, and needs nothing undone: the connector of pco runs on with the
new token, and the old one no longer works for anyone.

A secret rotated elsewhere, in the dashboard or through the API, needs no command either. The
daemon reads the run token again every five minutes, and at once when its connector logs that
Cloudflare refuses its token, and restarts the connector with the new one. Until then
`pco status` says `Cloudflare refuses the token its connector runs with`.

When the writer verdict is `foreign` as well, someone with the API token wrote a configuration
of their own into the tunnel; the order that puts that right is in
[Security](../security.md#the-connector-egress-filter): replace the token, rotate, recover, apply.

## Read on

- [Security](../security.md#a-connector-that-is-not-pcos): connectors that are not pco's.
- [Cloudflare token](../cloudflare-token.md#where-the-token-is-kept): the API token, the run
  token and where each is kept.
- Reference: [pco tunnel rotate](../cli/pco-tunnel-rotate.md).
