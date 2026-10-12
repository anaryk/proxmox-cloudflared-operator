# pco managed status

Show the managed network as the last cycle found it: whether it is enabled, the consent
mode and whether automatic attaches run, whether the vnet is as pco made it, the leg of this
node and the addresses left; the cards pco gave the guests, each with its state; the guests
that would get a card and why they do not, such as the gate not open yet, a grant the
appliance lacks (the pveum line is shown) or a firewall impact nobody confirmed; and the
cards on pconet no attachment explains, as a clone or a restore copies one, with what pco
did about each.

With `--json` the answer of the daemon is printed as the daemon sent it, re-indented, with
control and bidirectional characters escaped.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco managed status [flags]
```

## Examples

```text
# The managed network
pco managed status

# The attachments as JSON
pco managed status --json | jq '.attachments'
```

## Flags

```text
-h, --help   help for status
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco managed](pco-managed.md): Consent to cards on the managed network, and see who has one
- [Command reference](index.md): every command of pco, by group
