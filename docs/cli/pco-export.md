# pco export

Write the install to an encrypted bundle: its identity and the cluster it was made in, the
settings, manual routes, approvals and acknowledged segments, the registry of nodes, the CA
with its key, and the previous CA while a rotation runs, leader.json, and every other object
the controller writes: the networks and service addresses, the leases, consents and
attachments of the managed network, the claims, tombstones, placements and moves. With
`--credentials` the Cloudflare tokens are in it too. pco import restores the install from it on
another node or in another cluster, which is what a restore after a lost cluster or
controller, and the conversion of a node, need. Run tokens, bindings and the caches are left
out, and so is the engine memory, what the controller learns between its runs: the daemon
builds them again, the memory in the observe-only window after a restore.

It runs as root on the node and reads the store directly, while the daemon runs: nothing has to
be stopped. What it reads is the store at one moment; when the CA changed before the file was
written, as it does during a rotation, it refuses and is run again. On a host the store is
shared by the nodes of a cluster, and any node can export it.

The bundle is encrypted with AES-256-GCM under a key that Argon2id derives from a passphrase
of at least 16 characters, which makes a file that is attacked offline expensive. The
passphrase is read from the one line of `--passphrase-file`, which may be `/dev/stdin` for a
pipe, or asked for twice on a terminal without being shown. An empty answer has pco draw
one of 24 characters and show it once, after the file is written. It is never taken from an
argument, which others could read, and no log or error repeats it.

With its passphrase the bundle can impersonate the controller to every agent of the install,
as it holds the key of the CA that signs their certificates, and with `--credentials` it holds
the Cloudflare tokens: keep the passphrase apart from the file. The file is written for root
only, and an existing file is never replaced.

## Usage

```text
pco export [--credentials] [--passphrase-file <path>] <file> [flags]
```

## Examples

```text
# Back up the install; the passphrase is asked for twice
pco export /root/pco.bundle

# With the Cloudflare tokens, the passphrase read from a file
pco export --credentials --passphrase-file /root/.pco-passphrase /root/pco.bundle
```

## Flags

```text
    --credentials              carry the Cloudflare tokens in the bundle as well
-h, --help                     help for export
    --passphrase-file string   read the passphrase from the one line of this file instead of asking for it
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
