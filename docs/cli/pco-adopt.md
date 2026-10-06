# pco adopt

Replace the record of someone else that holds a hostname pco publishes, or take back
a record of this install that lost its marker. The conflict is shown first, and the
question needs a terminal; a script passes `--yes`. The replacement waits for a run in
which the tunnel is verified and its connector ready.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco adopt <name> [flags]
```

## Examples

```text
# Replace the record of someone else that holds shop.example.com
pco adopt shop.example.com

# The same from a script, without the question
pco adopt shop.example.com --yes
```

## Flags

```text
-h, --help   help for adopt
-y, --yes    do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
