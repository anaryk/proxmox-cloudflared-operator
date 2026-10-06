# pco route manual add

Make a manual route that publishes a hostname to a guest, named as qemu/101 or lxc/120,
whose address is proven as for a route in its Notes, or to an IPv4 address inside the
manualCIDRs of the settings. The options are those of the Notes.
`--allow-node` lets a route to an address point at a service of a node, which the egress
filter lets the connectors reach; the network of the node has to be in manualCIDRs too,
and a route to a guest never may. Without `--id` the daemon gives the route an id of
eight hex digits. It is published from the next cycle.

With `--json` the route as made is printed as JSON.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco route manual add <hostname> (--guest <owner> | --address <ipv4>) --port <port> [flags]
```

## Examples

```text
# Publish status.example.com to port 9000 of 10.0.5.20, with 10.0.5.0/24 in manualCIDRs
pco route manual add status.example.com --address 10.0.5.20 --port 9000

# wiki.example.com to the HTTPS service on port 8443 of qemu/101, on its card net1
pco route manual add wiki.example.com --guest qemu/101 --port 8443 --https --no-tls-verify --via net1

# In the same prefix, with an id of your own and the Host header the service expects
pco route manual add api.example.com --address 10.0.5.21 --port 8080 --id api --host-header api.internal.example.com
```

## Flags

```text
    --address string       the IPv4 address the route goes to
    --allow-node           let a route to an address point at a service of a node
    --guest string         the guest the route goes to, as qemu/101 or lxc/120
-h, --help                 help for add
    --host-header string   the Host header the service is sent
    --https                the service speaks HTTPS
    --id string            the id of the route: 1 to 32 of a-z, 0-9 and -
    --no-tls-verify        do not check the certificate of an HTTPS service
    --port uint16          the port of the service
    --sni string           the server name the TLS of an HTTPS service is asked for
    --via string           for a guest: the NIC (net0 to net31) or the address to reach it on
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco route manual](pco-route-manual.md): Manual routes: hostnames published to a guest or an address without its Notes
- [Command reference](index.md): every command of pco, by group
