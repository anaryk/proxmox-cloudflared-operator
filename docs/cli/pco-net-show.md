# pco net show

Show each thing pco-net.service keeps in place and whether it is, and how many packets the
table inet pco_net rejected since it was loaded: each was sent to the service prefix
and would otherwise have looked for it beyond the appliance. It changes nothing, and exits
with 1 when anything is missing or not as pco loads it. It runs as root in the appliance.

## Usage

```text
pco net show [flags]
```

## Examples

```text
# In the appliance
pco net show

# On the node, for the appliance in container 120
pct exec 120 -- pco net show
```

## Flags

```text
-h, --help   help for show
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco net](pco-net.md): Show how the appliance keeps the service prefix to itself
- [Command reference](index.md): every command of pco, by group
