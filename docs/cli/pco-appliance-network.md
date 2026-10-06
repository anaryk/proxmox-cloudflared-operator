# pco appliance network

Grant the appliance a bridge or vnet, in short: pco appliance network allow is
pco appliance grant-network with the bridge and the VLAN in one argument.

## Usage

```text
pco appliance network [flags]
pco appliance network [command]
```

## Examples

```text
# Let the appliance attach a card to VLAN 20 of vmbr1
pco appliance network allow vmbr1:20 --vmid 120

# The whole of the plain bridge vmbr2, without a question
pco appliance network allow vmbr2 --vmid 120 --yes
```

## Commands

- [pco appliance network allow](pco-appliance-network-allow.md): Grant the appliance a bridge or vnet, or one VLAN of it

## Flags

```text
-h, --help   help for network
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
