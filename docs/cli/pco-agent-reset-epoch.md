# pco agent reset-epoch

Have the agent of this node forget the epoch it remembers, with the desired state kept with
it. The agent refuses a controller of an epoch below the one it remembers, which keeps a
restored store from writing until pco promote draws above it; when the store was restored on
purpose and this node's memory is what stands in the way, this forgets it instead. It takes
effect at the next start of pco: systemctl restart pco. Only root may, and the question needs
a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers this request for root alone; the exit
status is 2 when the daemon could not be asked.

## Usage

```text
pco agent reset-epoch [flags]
```

## Examples

```text
# Forget the epoch, then restart pco
pco agent reset-epoch && systemctl restart pco

# From a script
pco agent reset-epoch --yes
```

## Flags

```text
-h, --help   help for reset-epoch
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco agent](pco-agent.md): The agent of this node
- [Command reference](index.md): every command of pco, by group
