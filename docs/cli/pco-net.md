# pco net

The service prefix 198.18.0.0/16 never leaves the appliance: pco-net.service routes it
to the dummy device pco0 before the connectors start, and the nftables table
inet pco_net rejects whatever is still sent to it, with or without the route. The daemon
loads both again when they are changed. These commands run in the appliance and need root.

While pco runs, pco net load puts back what is missing. Never restart pco-net.service for
that: pco and every connector restart with it.

After a boot at which pco-net.service failed, pco and the connectors, which require it, did
not start, and pco net load starts neither. Once pco net show names nothing a load cannot
put back, systemctl start pco.service starts pco-net.service again and then pco, which
starts the connectors once it has proved the container is the appliance.

## Usage

```text
pco net [command]
```

## Examples

```text
# In the appliance: is the service prefix kept in it?
pco net show

# After a boot at which pco-net.service failed, once show names nothing a load cannot put back
systemctl start pco.service
```

## Commands

- [pco net show](pco-net-show.md): Show the device, route, rules and table that keep the service prefix in the appliance

## Flags

```text
-h, --help   help for net
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
