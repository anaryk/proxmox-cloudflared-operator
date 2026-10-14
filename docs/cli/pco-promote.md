# pco promote

Make this node the writer again after the store was restored or copied. The daemon writes
nothing while the store is older than what the agents remember, went back under the running
writer, or was made in another cluster; pco status says which. pco promote first shows what
differs between the store and what the agents and Cloudflare say: the epoch each agent
follows, the nodes, the revisions, the cluster, the generation of the sentinels. Confirmed,
it makes this cluster the store's and draws a new epoch above every sentinel of the install
at Cloudflare and every epoch an agent reported, and sets observe-only mode: pco plan shows
what the daemon would change, and pco apply lets it. It refuses when a sentinel cannot be
read, which could hold a higher generation. Host profile only; an appliance draws a new
epoch with pco appliance recover. Only root may, and the question needs a terminal; a script
passes `--yes`.

It asks the daemon through its socket, which answers this request for root alone; the exit
status is 2 when the daemon could not be asked.

## Usage

```text
pco promote [flags]
```

## Examples

```text
# See what differs, and promote this node once you agree
pco promote

# From a script that read the differences already
pco promote --yes
```

## Flags

```text
-h, --help   help for promote
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
