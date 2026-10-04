# Cloudflare token

pco talks to the Cloudflare API with an API token that you make in the Cloudflare
dashboard and give to the daemon. This page says what the token needs and why, how to
make it, how to read the check pco runs on it, and how pco names what it creates.

## What the token is used for

With the token pco:

- lists the accounts and zones the token sees, to find the zone of each hostname;
- creates one tunnel in each account that holds a zone with routes, writes its
  ingress configuration, and reads the token that a connector runs with;
- creates, changes and deletes one proxied `CNAME` record for each published
  hostname, and only records that carry its own marker;
- during a check, creates and removes a test record and a test tunnel.

It does not need, and you should not grant, anything else: no zone settings, no
cache, no Access, no account settings, no permission to make other tokens, and not
the Global API Key.

## Permissions

| Permission in the dashboard | What pco does with it | Shown in the check as |
|---|---|---|
| Account > Cloudflare Tunnel > Edit | Create the tunnel, write its configuration, read its run token, delete test tunnels and, on uninstall, the tunnel itself. | `tunnel.read`, `tunnel.write` |
| Zone > DNS > Edit | Create, retarget and delete the CNAME records, and the test record. | `dns.read`, `dns.write` |
| Zone > Zone > Read | List the zones. | `zones` |

Under Account Resources, include the account that holds your zones. Under Zone
Resources, include the zones you publish in: either the zones one by one, or all zones
of that account. A token scoped to one zone cannot publish anything in another.

## Making the token

Both kinds of token work: a token owned by a user, and a token owned by an account.
A user token acts as you and stops working when you lose access to the account; an
account token is a durable service credential. pco asks Cloudflare to verify the token
as a user token first and, if that is refused, as an account token. Account tokens are
not yet confirmed against a real account, so if the check refuses yours, say so in an
issue.

1. Log in to the Cloudflare dashboard. For a user token go to My Profile, API
   Tokens. For an account token select the account and go to Manage account, Account
   API tokens.
2. Select Create Token, then Create Custom Token (the last choice on the page of
   templates).
3. Give it a name that says where it is used, for example `pco on pve1`.
4. Under Permissions add three rows:
   - Account, Cloudflare Tunnel, Edit
   - Zone, DNS, Edit
   - Zone, Zone, Read
5. Under Account Resources choose Include and the account. Under Zone Resources choose
   Include and Specific zone with the zones to publish in, or All zones from the
   account.
6. Optionally set a time to live. pco warns 30 days before the token expires, and the
   dashboard also lets you restrict the addresses it may be used from; if you set one,
   it has to be the address the node reaches Cloudflare from.
7. Select Continue to summary, check the summary, and select Create Token.
8. Copy the token now. Cloudflare shows it once.

Then give it to the daemon:

    pco credential add --label main

The token is asked for without being shown. In a script, `--token-file <file>` reads it
from a file (pco warns when the file can be read by others) or standard input does. It is
never taken from an argument or from the environment, which other users could read. A
token that a stored credential already has is refused, because two credentials with one
token would see the same zones.

`pco setup` accepts a token as well, while the daemon is stopped; see
[Quickstart](quickstart.md).

## Reading the check

`credential add` and `credential check <id>` print a list like this one, and the daemon keeps
the result of its own daily check of each stored token for `pco credential list` and
`pco status`:

    Credential main (a1b2c3d4)
    Token:     active, expires 2026-10-13T14:00:00+02:00
    Accounts:  Main
    Zones:     example.com (active)

      ✓ token
      ✓ accounts
      ✓ zones
      ✓ dns.read on example.com
      ✓ tunnel.read on Main

    Usable:    yes
    Write access was not tried. Run pco credential check a1b2c3d4 --deep to prove it.

- `token` is the token itself: that Cloudflare verifies it and it is active, with the
  expiry date when it has one.
- `accounts` and `zones` are what the token sees, listed above the checks. No account
  and no zone is a failure.
- `dns.read on <zone>` is a read of the records in that zone, and `tunnel.read on
  <account>` a lookup of the tunnel of this install in that account. Both are checked
  for every active zone, and for the account of every active zone.
- A failed line says what to grant. This is the one for a token without the DNS
  permission:

      ✗ dns.read on example.com
          grant Zone > DNS > Read on example.com

  Any other failure is shown as the error that Cloudflare or the network gave. When
  Cloudflare did not answer, with a network error, a server error or a rate limit, the
  line is marked `?` and starts with `Cloudflare did not answer`: that says nothing about
  the token, and `Usable` reads `not known, Cloudflare did not answer`.
- `Usable: yes` means the token is active, at least one zone is active, and every check
  that concerns an active zone or its account passed. A zone that is not active, for
  example one that is pending, fails its own line and does not make the token unusable
  while another zone is active. Such a zone is not served.

The plain check only reads. Write permissions cannot be read from a token, so
`pco credential check <id> --deep` proves them by making and removing things:

      ✓ dns.write on example.com
      ✓ tunnel.write on Main

A deep check creates a `TXT` record `_pco-probe-<suffix>.<zone>` and a tunnel
`pco-<id>_probe_<suffix>`, deletes each at once, and asks first (it needs a terminal, or
`--yes`). If one of them cannot be deleted, the line says `probe record left behind` or
`probe tunnel left behind` with its name, and it is yours to delete; a later check
lists leftover probe records under the checklist. The record the probe makes carries
the comment `pco:<id> probe`, which is how a later run tells it from a record that
publishes a hostname. `pco setup` runs the deep check on the token it is given before it
stores it.

`pco credential list` shows the stored credentials with the outcome of the last check
(`usable`, `problem` or `unknown`) and a note, such as the first failed check or the
expiry. `pco status` shows the same in its credentials table. The daemon checks every
token again about once a day, and again after 15 minutes when a check found it
unusable; those checks only read. A check that got no answer from Cloudflare leaves the
result of the one before in place, adds a warning to the event log and is repeated after
15 minutes.

A check that only reads still makes calls: the three that open it, one read of this
install's records in every active zone, and one lookup of the tunnel in every account the
zones belong to. With many zones that is many calls once a day, and they count against the
300 in five minutes of the credential.

## Several accounts and zone pins

You can store any number of credentials, for several accounts or several tokens for one
account. pco creates one tunnel in each account that holds a zone with routes, named the
same way in every one (the next section). A DNS record can only point at a tunnel of its
own account, which is why there is one tunnel per account and not one tunnel for all.

A zone that more than one credential sees is a problem pco does not decide for you. It
keeps serving the zone through the credential that served it alone before and puts a
line in `pco status`, with the way out:

    zone example.com is visible through credentials a1b2c3d4 and e5f6a7b8; pin it with
    zonePins (a1b2c3d4 serves it until then)

If no credential served the zone alone before, the account of that zone is left as it
is until you pin it. A pin is a line in the settings that names a zone and the
credential that serves it:

    "zonePins": { "example.com": "a1b2c3d4" }

[Operations](operations.md) says where the settings are and how to change them. A pin to
a credential that does not see the zone, and a zone that disappeared from the listing of
its credential, also leave the account as it is, with a message that says which. Its
routes then have the state `frozen`, and nothing is changed at Cloudflare for that
account until the zone is listed again, the pin is fixed, or `pco apply --confirm-deletes`
confirms that the zone is gone.

## What pco names and marks

Everything pco makes at Cloudflare can be told from your own objects by name or marker,
and pco changes only what carries them. The install id is the 12 characters that
`pco setup` made (it is in the output of the setup, and in the name of every tunnel).

| Object | Name or marker |
|---|---|
| Tunnel | `pco-<install id>`, one in each account that serves a zone. It is a remotely managed tunnel: its configuration lives at Cloudflare and pco is its only writer. |
| DNS record | A proxied `CNAME` at the hostname, pointing at `<tunnel id>.cfargotunnel.com`, with a comment that begins with `pco:<install id>`. |
| Sentinel rule | The last rule but one of the tunnel's ingress is `g<generation>.<nonce>.pco-<install id>.invalid`, which answers 404 and whose name can never resolve. It records which writer wrote the configuration, so that pco notices a second installation using the same id or a stale writer, and stops instead of overwriting. |
| Catch-all rule | The last rule of the tunnel answers 404. |
| Blocking rules | A hostname that is claimed but not served, such as one of a guest that is stopped, below the identity minimum or waiting for approval, has a rule that answers 503, so that no wildcard of someone else serves it. |
| Test objects | `_pco-probe-<suffix>.<zone>` and `pco-<install id>_probe_<suffix>`, of a deep check. |

A DNS record without the marker is never edited or deleted. If someone else's record
holds a hostname that you publish, it stays and the route waits; see `pco adopt` in
[Troubleshooting](troubleshooting.md).

pco owns its tunnel. A change you make to its configuration in the dashboard is not
supported and is overwritten in the next cycle, and so is any setting in it that pco does
not manage, with one exception: a configuration whose sentinel names a newer generation of
this install is taken for the work of a newer writer, and pco stops writing. A token that
can edit the tunnel can do that, and can also read the tunnel's run token and run a
connector of its own; [Security](security.md) says what pco does then and what to do.

## Rotating a token

pco has no command that replaces the secret of a credential. To rotate a token:

1. Make the new token as above and add it with a new label:
   `pco credential add --label main-2`. If the old credential came from `pco setup`, its
   label is `setup`; `pco credential list` shows the ids.
2. While both are valid, the zone is seen through two credentials. The one that served
   it keeps it and `pco status` shows the message above. Pin the zone to the new
   credential in the settings, as shown above, and wait for the message to go from
   `pco status`. This step is not optional: the old credential would otherwise go on
   serving the zone after its token is revoked, and every call through it would fail.
3. Revoke the old token in the dashboard, or let it expire.
4. Remove the old credential at once: `pco credential remove <id>`.

The last step is refused while the credential can still reach anything of this install:
a record of it in a zone the token sees, or its tunnel in an account the token sees.
That is why the old token has to be revoked first. A token that Cloudflare no longer
accepts reaches nothing, so it is removed, and the event log notes that what it managed
could not be checked.

Do not leave the dead credential in place. While the daemon runs, a credential whose
token stopped working keeps the list of zones it had and fails its calls. After a restart
it has no list, cannot get one, and holds the whole cycle for every account until it is
removed (see below).

## Limits and expiry

pco spends at most 300 requests in 5 minutes on each credential, with room for 20 at
once. When Cloudflare answers 429 it stops calling for as long as Cloudflare asked and
fails calls fast meanwhile, with `not sent: holding back after an earlier 429`; what could
not be done is tried again in a later cycle.

A token that has expired, or was revoked, cannot be used for anything: every call with
it fails, so pco can change nothing at Cloudflare through it, and `pco status` and
`pco doctor` show the credential as a problem. The tunnel and its connector keep serving
what was published, because the connector runs on a token of its own that Cloudflare gave
the tunnel.

What a dead token does to the other credentials depends on whether the daemon has listed
its zones. A daemon that has, keeps that list, uses it, and puts a line in the problems
(`credential <id>: listing its zones failed (...); using the list from ...`). A daemon that
has not, such as one that was restarted after the token died, or one that was given a token
that never worked, holds the whole cycle: `credential <id>: its zones are not listed yet
(...); nothing is changed at Cloudflare until they are`. That holds every account, also
those that other credentials serve, until the zones are listed or the credential is
removed with `pco credential remove <id>`. A zone that is in doubt, such as one that
several credentials see with no pin, freezes only its own account.

`pco status` and `pco doctor` warn when a token expires in less than 30 days. The warning
in `pco status` looks like this:

    Credentials:
      LABEL  STATE   NOTE
      main   usable  token expires 2026-10-13T14:00:00+02:00 (in 12 days)

## Where the token is kept

The token is stored in `/etc/pve/priv/pco/credentials/<id>.json`, a place that Proxmox
makes readable by root alone, and on a cluster replicates to every node. The daemon never
returns it through its socket and never writes it to a log; pco prints and encodes it as
`[redacted]`. [Security](security.md) has the full list of secrets and where they live.
