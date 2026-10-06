# pco apply

Leave observe-only mode: from the next cycle the daemon changes Cloudflare.

With `--confirm-deletes`, what the daemon shows waiting for a confirmation is listed first:
the DNS removals the mass delete guard holds back, guests that Proxmox no longer lists,
zones that left their listing and tunnels no credential sees. Confirmed, the daemon
accepts exactly what was listed, and refuses when that changed in the meantime: look
again and repeat. The question needs a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco apply [flags]
```

## Examples

```text
# Leave observe-only mode
pco apply

# See what waits for a confirmation, and accept it
pco apply --confirm-deletes

# The same from a script, without the question
pco apply --confirm-deletes --yes
```

## Flags

```text
    --confirm-deletes   accept what waits for a confirmation at the next run: held deletes, guests that vanished, zones and tunnels that are gone
-h, --help              help for apply
-y, --yes               do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
