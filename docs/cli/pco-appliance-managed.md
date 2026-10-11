# pco appliance managed

Make the managed network of the appliance from the node: the SDN vnet pconet and the grants
the appliance needs to put cards on it. pco appliance managed enable does it.

## Usage

```text
pco appliance managed [flags]
pco appliance managed [command]
```

## Examples

```text
# Make the managed network of the appliance in container 120
pco appliance managed enable --vmid 120

# Then, inside the appliance, take it into the store
pct exec 120 -- pco network managed enable
```

## Commands

- [pco appliance managed enable](pco-appliance-managed-enable.md): Make the SDN vnet of the managed network and grant it to the appliance

## Flags

```text
-h, --help   help for managed
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
