# pco cluster join-token

Make the token an appliance agent container of a node enrols with, once, within 15 minutes:
it names the controller's endpoints and the fingerprint of the install's CA, and carries a
secret the daemon keeps only the hash of. The token is written to the file `--file` names, which
only root may read, or printed once when the output is a terminal; it never goes into a log or
an event. On the node: pco appliance install `--kind` agent `--join-file` `<path>`. A token for a
contested node is refused; pco node forget ends that first.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco cluster join-token [flags]
```

## Examples

```text
# Into a file for pct push
pco cluster join-token --node pve3 --file /root/pve3.join

# Shown once on the terminal
pco cluster join-token --node pve3
```

## Flags

```text
    --file string   the file to write the token to, made with mode 0600; without it the token is printed once, on a terminal only
-h, --help          help for join-token
    --node string   the node the agent joins as
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco cluster](pco-cluster.md): The cluster: its status, the lease, join tokens, the upgrade
- [Command reference](index.md): every command of pco, by group
