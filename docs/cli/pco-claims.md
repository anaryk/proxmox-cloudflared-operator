# pco claims

A public hostname belongs to one owner at a time, a guest or a manual route: the one that
claimed it first holds it, and the others that name it wait in line. The daemon keeps the
claims in `/etc/pve/pco/claims` and settles them in every cycle. These commands show them,
and hand a hostname to another owner that claims it.

## Usage

```text
pco claims [command]
```

## Examples

```text
# Who holds each hostname, and who waits for it
pco claims list

# Hand www.example.com to qemu/102, which claims it too
pco claims resolve www.example.com qemu/102
```

## Commands

- [pco claims list](pco-claims-list.md): List the claims on the public hostnames
- [pco claims resolve](pco-claims-resolve.md): Hand a public hostname to another owner that claims it

## Flags

```text
-h, --help   help for claims
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
