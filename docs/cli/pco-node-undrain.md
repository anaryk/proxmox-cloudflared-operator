# pco node undrain

End the drain of a node: it is active again and runs the shared tunnel. A drain that Proxmox's
maintenance mode made ends with the mode, not here.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node undrain <node> [flags]
```

## Examples

```text
# After maintenance of pve2
pco node undrain pve2

# Where the drain came from
pco node list
```

## Flags

```text
-h, --help   help for undrain
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
