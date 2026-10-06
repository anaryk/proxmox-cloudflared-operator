# pco segment acknowledge

Acknowledge a segment: from the next cycle the routes at observed proven on it are served,
unless they wait for an approval of their guest. The routes it releases are shown first,
and the question needs a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco segment acknowledge <bridge>[:<vlan>] [flags]
```

## Examples

```text
# Serve the routes at observed on the untagged part of vmbr1
pco segment acknowledge vmbr1

# On VLAN 20 of vmbr1, from a script
pco segment acknowledge vmbr1:20 --yes
```

## Flags

```text
-h, --help   help for acknowledge
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco segment](pco-segment.md): Acknowledge the bridges and VLANs routes at observed may be served on
- [Command reference](index.md): every command of pco, by group
