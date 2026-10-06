# pco appliance recover

Draw a new writer epoch for this appliance, above the highest generation the sentinels of
its tunnels at Cloudflare carry and above the stored one, after a snapshot rollback or a
restore put an older state on its volume. It reads Cloudflare with the stored credentials,
so one must be stored (pco credential add). pco.service is stopped while it runs and
started after; the install only observes until pco apply. It runs as root inside the
appliance; on a host, pco setup `--recover` adopts an install after its store was lost.

## Usage

```text
pco appliance recover [flags]
```

## Examples

```text
# Inside the appliance, after a rollback to the snapshot taken before an upgrade
pco appliance recover

# From the node, for the appliance in container 120
pct exec 120 -- pco appliance recover
```

## Flags

```text
-h, --help   help for recover
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
