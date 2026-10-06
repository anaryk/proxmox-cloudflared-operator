# pco netns

pco reaches a network the node has no address in through a leg of its own in that network.
The legs live in the network namespace pco, which a veth pair links to the host; the
connectors dial a service address of 198.18.0.0/16 for each target behind a leg, and the
namespace NATs it to the target through its leg. pco-netns.service brings the namespace back at
boot from what the daemon last applied, before the guests start; the daemon keeps it after
that. The unit has no stop action: a restart only brings up again what is missing and leaves
the legs and the open connections as they are. These commands run on the node and need root.

## Usage

```text
pco netns [command]
```

## Examples

```text
# Whether the namespace, its legs and its tables are as pco left them
pco netns status

# After a change made by hand: bring back what pco-netns.service brings up
systemctl restart pco-netns
```

## Commands

- [pco netns status](pco-netns-status.md): Show the legs of the gateway namespace and whether it is as pco left it

## Flags

```text
-h, --help   help for netns
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
