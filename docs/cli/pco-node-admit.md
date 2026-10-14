# pco node admit

Admit a node that joins. An appliance agent is trusted by the fingerprint of its certificate:
`--fingerprint` takes at least eight hex digits of the SHA-256 that pco agent status shows in the
container (pct exec `<vmid>` -- pco agent status), and they must be the beginning of the one
enrolled. A node that stays joining because more shared routes would move to pinned tunnels
than cluster.admissionMaxMoves allows is admitted all the same: every route it fails moves
first. A contested node is not admitted: pco node forget ends that, and a new token enrols it
again. `--renew`, to let an appliance agent whose certificate expired renew it once, is refused
until the appliance install can renew the certificate of an agent; a new join token enrols it.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco node admit <node> [flags]
```

## Examples

```text
# An appliance agent, by its fingerprint
pco node admit pve3 --fingerprint 9f86d081884c7d65

# A host node that would move too many routes
pco node admit pve2
```

## Flags

```text
    --fingerprint string   at least eight hex digits of the SHA-256 of the agent's certificate, as pco agent status shows it
-h, --help                 help for admit
    --renew                let an appliance agent whose certificate expired renew it once (refused until the appliance install can)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco node](pco-node.md): The nodes of the install: list, admit, drain, forget
- [Command reference](index.md): every command of pco, by group
