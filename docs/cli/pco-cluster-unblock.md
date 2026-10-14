# pco cluster unblock

End the block of an address that every node blocks because an agent's forwarding-table watch
saw the MAC of its guest on the port of another guest, or leave the guest's port while the
guest did not migrate. A block ends by itself ten minutes after the last report of it, once
the guest's node attests the card again; this ends it now, when what blocked it is found and
gone. pco cluster status counts the blocks; pco status `--json` lists them under
cluster.blocks.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco cluster unblock <address> [flags]
```

## Examples

```text
# End the block of an address
pco cluster unblock 10.0.0.7

# The blocks
pco status --json | jq '.cluster.blocks'
```

## Flags

```text
-h, --help   help for unblock
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco cluster](pco-cluster.md): The cluster: its status, the lease, join tokens, the upgrade
- [Command reference](index.md): every command of pco, by group
