# pco segment

A route proven at the observed level is served only on a segment, a bridge and a VLAN, an
admin acknowledged: on a new one nothing is served until then. A segment is named as
vmbr1 for the untagged part of a bridge and as vmbr1:20 for VLAN 20 of it.

## Usage

```text
pco segment [command]
```

## Examples

```text
# The segments routes at observed were proven on
pco segment list

# Serve the routes at observed on VLAN 20 of vmbr1
pco segment acknowledge vmbr1:20
```

## Commands

- [pco segment acknowledge](pco-segment-acknowledge.md): Serve the routes at observed on a segment
- [pco segment list](pco-segment-list.md): List the segments routes at observed were proven on, and those acknowledged
- [pco segment revoke](pco-segment-revoke.md): Take the acknowledgement of a segment back

## Flags

```text
-h, --help   help for segment
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
