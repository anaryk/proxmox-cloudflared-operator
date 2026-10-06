# Use several tokens and accounts

This guide adds a second Cloudflare token, for a zone in another account, decides which token
serves a zone that both see, and removes a token again. The first credential in the examples is
`a1b2c3d4` (`main`), for `example.com` in the account Main; the second is `e5f6a7b8`
(`second`), which sees `example.com` too and `example.net` in the account Second.

## When you need it

- Your zones are in more than one Cloudflare account.
- You keep one token for each zone, or for each group of zones, so that a token that leaks
  reaches less.
- You replace a token, and the old and the new one stand side by side for a while;
  [Cloudflare token](../cloudflare-token.md#rotating-a-token) has that order.

## How pco uses several credentials

Each stored token is a credential. pco lists the accounts and zones of each, and serves every
zone through one credential. In each account that holds a zone with routes it makes one tunnel,
`pco-<install id>`, the same name in every account, because a DNS record can point only at a
tunnel of its own account. So two accounts mean two tunnels and two connectors on the node.

A zone that two credentials see is a question pco does not answer for you. The credential that
served the zone alone before keeps it, and a problem line says so, until a pin in the setting
`zonePins` names the credential that serves it. A zone that no credential served before is left
as it is until it is pinned.

## Before you start

- Make the new token as [Cloudflare token](../cloudflare-token.md#making-the-token) says, with
  the account and the zones it is for, and keep it in a file only root can read.
- Credentials of one Cloudflare user share Cloudflare's limit of 1200 requests in 5 minutes,
  whatever token they come with. See step 5.

## Steps

1. Add the token. It is checked first, and stored only when it can do what pco needs:

   ```sh
   pco credential add --label second --token-file /root/cf-token-second
   ```

   The check lists what the token sees and what it may read:

   ```text
   Credential second (e5f6a7b8)
   Token:     active, expires 2027-01-01T01:00:00+01:00
   Accounts:  Main, Second
   Zones:     example.com (active), example.net (active)

     ✓ token
     ✓ accounts
     ✓ zones
     ✓ dns.read on example.com
     ✓ dns.read on example.net
     ✓ tunnel.read on Main
     ✓ tunnel.read on Second

   Usable:    yes
   Write access was not tried. Run pco credential check e5f6a7b8 --deep to prove it.

   Added credential e5f6a7b8 (second).
   ```

   A failed line names what to grant, as `grant Zone > DNS > Read on example.net`, and a zone
   the token lists but may not read is left out of it, as `example.net left out: no DNS
   read`. A token that a stored credential has already is refused.

2. Prove that it may write, with a test record and a test tunnel that are removed at once:

   ```sh
   pco credential check e5f6a7b8 --deep
   ```

3. Publish a hostname of the new zone, as in [Publish a container](publish-a-container.md).
   The first route in the account Second makes its tunnel, and `pco status` shows both:

   ```text
   Tunnels:
     NAME              ID        VERIFIED  CONNECTOR
     pco-7f3a9c0d41b2  0a1b2c3d  yes       active, ready, 4 connections
     pco-7f3a9c0d41b2  5e6f7a8b  yes       active, ready, 4 connections
   Credentials:
     LABEL   STATE   NOTE
     main    usable  -
     second  usable  -
   ```

4. Both credentials see `example.com`. `main` served it alone before and keeps it, and
   `pco status` says so among its problems:

   ```text
   Problems:
     - zone example.com is visible through credentials a1b2c3d4 and e5f6a7b8; pin it with zonePins (a1b2c3d4 serves it until then)
   ```

   Decide which credential serves it and pin it. The pin names the credential by its id,
   which `pco credential list` shows:

   ```sh
   pco settings show --json > /root/pco-settings.json
   jq '.settings.zonePins = {"example.com": "a1b2c3d4"}' /root/pco-settings.json > /root/pco-settings.new.json
   pco settings apply /root/pco-settings.new.json
   ```

   A pin to the other credential moves the zone, with its hostnames on the tunnel, to it in
   the next cycle. A pin to a credential that does not see the zone freezes the account of the
   zone: its routes are `frozen`, and nothing is changed at Cloudflare for that account until
   the pin is fixed.

5. When both tokens belong to one Cloudflare user, keep their spending below the 1200 they
   share. `cloudflareBudget` applies to each credential, 1000 by default, so set it to 600 or
   less with two, the same way as the pin. It is read when the daemon starts:

   ```sh
   systemctl restart pco
   ```

   The connectors keep running through the restart.

## Check

```sh
pco credential list
pco status
```

```text
ID        LABEL   KIND    STATE   NOTE
a1b2c3d4  main    scoped  usable  -
e5f6a7b8  second  scoped  usable  -
```

The problem line about the zone is gone once it is pinned. The daemon checks every token again
about once a day, and a check that fails shows in the `STATE` and `NOTE` of the credential, in
`pco status` and in `pco doctor`. `pco credential check <id>` checks one at once, as after you
granted what a failed line asked for.

With a tunnel in each account, a command that acts on one tunnel asks which:

```text
pco: invalid request: the install has tunnels in accounts 0123456789abcdef0123456789abcdef and fedcba9876543210fedcba9876543210; name one with --account
```

## Undo

`pco credential remove` refuses while the token can still reach a record of this install in a
zone it sees, or the tunnel of this install in an account it sees, and names them:

```text
pco: refused: credential e5f6a7b8 still manages record blog.example.net in zone example.net, tunnel pco-7f3a9c0d41b2 in account fedcba9876543210fedcba9876543210
```

A token Cloudflare no longer accepts reaches nothing, so to stop using the account Second:

1. Take the hostnames of its zones out of the Notes, and wait until their records are deleted,
   after the grace period of a minute: `delete-record` in `pco events`. Once the token is gone,
   nothing on the node can delete them.
2. Revoke the token at Cloudflare, then remove the credential:

   ```sh
   pco credential remove e5f6a7b8
   ```

3. No credential sees the tunnel of the account Second any more. Its connector is kept, and
   `pco plan` lists the tunnel among what waits for a confirmation. Confirm it, which removes
   the connector on the node:

   ```sh
   pco apply --confirm-deletes
   ```

4. Delete the tunnel `pco-7f3a9c0d41b2` of the account Second at Cloudflare, now that no
   connector holds it. pco does not delete it: it deletes its tunnel only on
   `pco uninstall --purge-cloudflare`.
5. Take a pin that names the credential out of `zonePins`.

Do not leave a credential whose token stopped working: after a restart of the daemon it holds
every cycle, for every account, until it is removed
([Cloudflare token](../cloudflare-token.md#limits-and-expiry) says why).

## Read on

- [Cloudflare token](../cloudflare-token.md): the permissions, the check, the pins, and the
  order for replacing a token.
- [Settings](../settings.md#cloudflare): `zonePins` and `cloudflareBudget`.
- Reference: [pco credential add](../cli/pco-credential-add.md),
  [pco credential check](../cli/pco-credential-check.md),
  [pco credential remove](../cli/pco-credential-remove.md).
