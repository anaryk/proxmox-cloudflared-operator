# pco tunnel rotate

Give the tunnel of the install a new secret at Cloudflare and end the connections of all its
connectors. The connector pco runs on this node restarts with the new token at once; a
connector elsewhere, started with a token that was read with a stolen API token, loses its
session and cannot connect again. Do it when pco status names a connector that you do not
run. It refuses while pco is in observe-only mode, which changes nothing at Cloudflare; pco apply
ends that mode. Only root may, and the question needs a terminal; a script passes `--yes`.

It asks the daemon through its socket, which answers this request for root alone; the exit
status is 2 when the daemon could not be asked.

## Usage

```text
pco tunnel rotate [flags]
```

## Examples

```text
# Rotate the secret of the tunnel of the install
pco tunnel rotate

# Of its tunnel in one account, when it has tunnels in several
pco tunnel rotate --account 0123456789abcdef0123456789abcdef
```

## Flags

```text
    --account string   the account of the tunnel; needed when the install has tunnels in several
-h, --help             help for rotate
-y, --yes              do not ask for confirmation (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco tunnel](pco-tunnel.md): Act on the tunnels of the install
- [Command reference](index.md): every command of pco, by group
