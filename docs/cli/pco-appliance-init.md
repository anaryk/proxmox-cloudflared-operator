# pco appliance init

Make the store of this appliance from the bootstrap the installer on the node pushed into
it, and remove the bootstrap, which holds the secrets. pco appliance install and repair run
it; it runs as root inside the appliance. A bootstrap made for another container, by its
MACs or the VMID its state volume names, changes nothing, and is removed. In mode install
the daemon must not run, and a bootstrap turned away for that stays for the next run; in
modes repair and recover pco.service is stopped first. Every mode restarts pco.service at
the end. A step that fails is named, and an init of the same mode finishes what it left.

## Usage

```text
pco appliance init [flags]
```

## Examples

```text
# Inside the appliance, as the installer runs it
pco appliance init --bootstrap /var/lib/pco/bootstrap.json

# From the node, for the appliance in container 120
pct exec 120 -- pco appliance init --bootstrap /var/lib/pco/bootstrap.json
```

## Flags

```text
    --bootstrap string   the bootstrap the installer pushed, as /var/lib/pco/bootstrap.json
-h, --help               help for init
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
