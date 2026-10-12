# pco appliance managed enable

Make the managed network of an appliance on this node: pco's own SDN vnet pconet in the
zone pco of type simple, without a subnet and without isolated ports, as the appliance's own
card is a port of it and the guest firewall isolates the guests. First it reads the node's
firewall: it refuses the nftables backend (nftables: 1 in the node's firewall options), a
datacenter firewall that is off, and pve-firewall not running, each before anything is made.
Then it takes the lock of the SDN configuration, refuses changes of others that are pending
unless `--confirm-sdn-apply` applies them along, makes the zone and the vnet where they are
missing, each noted in the appliance's manifest first, and asks before the apply, which
reloads the network on every node. Last it grants the appliance the vnet as grant-network
does: role PCOManaged on the appliance and role PCOSDN on `/sdn/zones/pco/pconet`, for user
pco@pve and the appliance's token. All of it is shown and asked about once, before anything is
made; `--yes` answers that.

`--pool` `<name>` grants role PCOManaged on `/pool/<name>` as well: the appliance's token may then
change the network cards and the firewall of every guest in that pool, so that automatic
consent attaches them without a pveum line per guest. It is never granted on /.

The appliance must run, as its manifest notes what is made. Inside it, pco network managed
enable then takes the network into the store. It runs as root on the node and exits 0 once
the vnet is there and granted, 1 otherwise.

## Usage

```text
pco appliance managed enable [flags]
```

## Examples

```text
# Make the managed network of the appliance in container 120
pco appliance managed enable --vmid 120

# Without a question, and grant the cards of the guests in pool web too
pco appliance managed enable --vmid 120 --pool web --yes
```

## Flags

```text
    --confirm-sdn-apply   apply the changes of others that are pending in the SDN along with pco's
-h, --help                help for enable
    --pool string         grant the appliance the network cards and the firewall of every guest in this pool as well
    --vmid int            the VMID of the appliance
-y, --yes                 apply and grant without asking (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance managed](pco-appliance-managed.md): Make the managed network of the appliance
- [Command reference](index.md): every command of pco, by group
