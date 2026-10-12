# pco managed revoke

Take back the consent given with pco managed approve. A card the guest has goes after
the grace managed.detachGrace, unless the tag cf-tunnel-managed or automatic consent keeps
it; consent back within the grace keeps the card. The question needs a terminal; a
script passes `--yes`.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco managed revoke <owner> [flags]
```

## Examples

```text
# Take back the consent of lxc/120
pco managed revoke lxc/120

# The same from a script, without the question
pco managed revoke lxc/120 --yes
```

## Flags

```text
-h, --help   help for revoke
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco managed](pco-managed.md): Consent to cards on the managed network, and see who has one
- [Command reference](index.md): every command of pco, by group
