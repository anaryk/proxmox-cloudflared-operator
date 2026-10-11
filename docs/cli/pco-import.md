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
apply. leader.json comes back as it was, so the controller writes nothing until pco promote
draws an epoch above the one of the install that was lost. The install remembers the cluster
it was made in: restored in another, the daemon writes nothing until pco promote makes that
cluster its writer, and the nodes of the lost cluster are admitted again after it.

Run it before pco setup on the node that is to run the controller, which keeps the install it
finds, and then run pco promote. It restores the install of a host: the install of an
appliance records the container it runs in, which a bundle does not carry.

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
