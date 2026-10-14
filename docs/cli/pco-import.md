# pco import

Restore the install of a bundle that pco export wrote, on a node where pco has none: its
identity, settings, manual routes, approvals, acknowledged segments, the registry of nodes,
the CA with its key and the previous CA of a rotation, leader.json, the networks, service
addresses, leases, consents, attachments, claims, tombstones, placements and moves, and the
Cloudflare credentials the bundle carries. It runs as root, with the daemon stopped, and
works on the store directly. A store that holds an install is refused and left as it is,
and so is a bundle that does not open or read: a restore that stops half way stores no
install, and is run again.

The passphrase is read from the one line of `--passphrase-file`, or asked for on a terminal
without being shown; it is never taken from an argument. A wrong passphrase and a file that
was changed cannot be told apart.

The install is stored in observe-only mode whatever it was: it publishes nothing until pco
apply, and the day in which the agent endpoint takes a certificate the store has no enrolment
for begins with the restore, whenever the exported install began to observe. What the
controller learns between its runs, the engine memory, is in no bundle: it is learnt again
in the observe-only window, before anything is removed. A bundle of an earlier shape carries
no pool of service addresses; the pool then starts above the addresses in use, and an
address that had been released may be given out again.

leader.json comes back as it was, so the controller writes nothing until pco promote
draws an epoch above the one of the install that was lost. The install remembers the cluster
it was made in. Restored in that cluster, the nodes that run pco take it up as it is.
Restored in another, as after the whole cluster was lost, the first node pco setup runs on
takes it into the new cluster: setup refuses while a connector of the install is still
connected at Cloudflare, which says that the old cluster runs it, then makes a new CA, so
that nothing the nodes of the old cluster hold is trusted, and removes those nodes from the
registry. The daemon writes nothing until pco promote on that node makes the new cluster
the writer; every other node enrols again with pco setup after the promotion, a node of the
old name under its old index.

Run it before pco setup on the node that is to run the controller, which keeps the install it
finds and starts the daemon, and then run pco promote there. It restores the install of a
host: the install of an appliance records the container it runs in, which a bundle does not
carry.

## Usage

```text
pco import [--passphrase-file <path>] <file> [flags]
```

## Examples

```text
# Restore the install on a node where pco is not set up yet
pco import /root/pco.bundle

# With the passphrase read from a file
pco import --passphrase-file /root/.pco-passphrase /root/pco.bundle
```

## Flags

```text
-h, --help                     help for import
    --passphrase-file string   read the passphrase from the one line of this file instead of asking for it
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
