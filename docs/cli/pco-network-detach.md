# pco network detach

Detach a network, named by its id as pco network list shows it (vmbr1, vmbr1.20): its leg is
removed at once, its service addresses are released, and the routes served through it are
no longer served that way. The routes served through it are shown first, and the question
needs a terminal; a script passes `--yes`. The last network detached disables
pco-netns.service, which takes nothing down: the namespace stays until pco is uninstalled.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco network detach <id> [flags]
```

## Examples

```text
# Detach vmbr1, after a look at what it serves
pco network detach vmbr1

# VLAN 20 of vmbr2, from a script
pco network detach vmbr2.20 --yes
```

## Flags

```text
-h, --help   help for detach
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
- [Command reference](index.md): every command of pco, by group
