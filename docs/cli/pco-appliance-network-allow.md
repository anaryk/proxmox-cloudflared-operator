# pco appliance network allow

The same as pco appliance grant-network `--bridge` `<bridge>` \[--vlan `<vlan>]:` it grants the
appliance the bridge or vnet, or the VLAN after the colon, with the lines, the question and
the record in the manifest of grant-network. It runs as root on the node and exits 0 once
the grant holds, 1 otherwise.

## Usage

```text
pco appliance network allow <bridge>[:<vlan>] [flags]
```

## Examples

```text
# The plain bridge vmbr1
pco appliance network allow vmbr1 --vmid 120

# VLAN 20 of the VLAN-aware bridge vmbr2, without a question
pco appliance network allow vmbr2:20 --vmid 120 --yes
```

## Flags

```text
-h, --help       help for allow
    --vmid int   the VMID of the appliance
-y, --yes        do not ask (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance network](pco-appliance-network.md): Grant the appliance a bridge or vnet, in short
- [Command reference](index.md): every command of pco, by group
