# pco node forget

Remove a node that is gone from the install: its entry becomes removed, so that its agent is
refused from its next request, its connections are closed, its pinned tunnels are deleted and
the shared tunnel's secret is rotated (cluster.rotateOnForget), so that the run token the node
keeps stops working, and its connector is dropped at Cloudflare; the remaining replicas keep
their connections until each restarts with the new token. With cluster.rotateOnForget off
the token keeps working, and the connectors the node reported are dropped at Cloudflare by
their ids whenever they connect again. A host agent that comes back reads its removed entry
in the registry and stands down: it stops its connectors and serves nothing. What cannot
be done at once, the next cycles finish; in observe-only mode they wait for pco apply.
Replacing a node with the same name: forget it, pvecm delnode and pvecm add for the new
machine, then pco setup on it, or a new join token.

`--compromised` is for a node whose disk is out of your hands: it held every credential of the
install. It is refused while the node is still in the Proxmox cluster, which would hand it
every new secret: pvecm delnode first. Then every secret is rotated, step by step, in the
order that keeps each new one from the node: you replace the Cloudflare API tokens (pco
credential add, then remove the old ones and revoke them at Cloudflare) and give the Proxmox
token a new secret (pco setup `--repair`); the daemon then rotates every tunnel's secret, waits
for the connectors, rotates the CA of the install and, ten minutes later, stops taking the
old one. pco cluster status follows it; a controller that takes over in the middle finishes
it. What the disk holds that pco cannot rotate is listed. With `--compromised` it runs as root
on the controller. The question needs a terminal; a script passes `--yes`.

An appliance agent's container holds less: the run tokens of its tunnels and its own Proxmox
token, which holds the reading role PCO alone. `--compromised` for one is refused while that
token is there: pco appliance uninstall `--vmid` `<vmid>` on its node removes the container and the
token first. A token removed by hand while the container runs is taken as gone: the removed
entry makes the agent endpoint refuse the container's certificate, so it gets no new secret.
Then the secrets of the tunnels are rotated, whatever cluster.rotateOnForget says; the
credentials, the controller's Proxmox secret and the CA stay, as the container never held them.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node forget <node> [flags]
```

## Examples

```text
# A node that was destroyed
pco node forget pve3

# A node whose disk was stolen, once pvecm delnode ran
pco node forget pve3 --compromised
```

## Flags

```text
    --compromised   the node's disk is out of your hands: rotate every secret of the install
-h, --help          help for forget
-y, --yes           do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
