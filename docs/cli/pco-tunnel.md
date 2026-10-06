# pco tunnel

Act on the tunnels of the install at Cloudflare, one for each account that holds a zone
with routes. The daemon makes and keeps them; pco status lists them with their connectors.

## Usage

```text
pco tunnel [command]
```

## Examples

```text
# Give the tunnel of the install a new secret
pco tunnel rotate

# The tunnels and their connectors
pco status
```

## Commands

- [pco tunnel rotate](pco-tunnel-rotate.md): Give a tunnel a new secret, so that no connector but pco's runs it

## Flags

```text
-h, --help   help for tunnel
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
