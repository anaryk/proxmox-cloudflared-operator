# pco egress

The egress filter is the nftables table inet pco_egress. It lets the processes of the user
pco-connector, which the connectors run as, open connections only to the targets the
daemon verified, to the resolvers of the node on port 53 and to Cloudflare's edge, so that
whoever changes a tunnel at Cloudflare cannot point a connector at anything else the node
reaches. These commands work on this node directly, without the daemon, and need root.

pco egress load loads the table again when it is gone or not as it should be, and keeps the
sets of an intact one. Never restart pco-egress.service for that: every connector restarts
with it.

## Usage

```text
pco egress [command]
```

## Examples

```text
# Whether the filter is on, and what it holds
pco egress show

# Keep the connectors from 10.0.0.12, whatever the daemon verified
pco egress block 10.0.0.12
```

## Commands

- [pco egress block](pco-egress-block.md): Keep the connectors from an address, whatever the daemon verified
- [pco egress off](pco-egress-off.md): Switch the egress filter off, when it is itself the fault
- [pco egress on](pco-egress-on.md): Switch the egress filter back on
- [pco egress show](pco-egress-show.md): Show whether the egress filter is on, what its sets hold and what it rejected
- [pco egress unblock](pco-egress-unblock.md): Take an address off the block list of this node

## Flags

```text
-h, --help   help for egress
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
