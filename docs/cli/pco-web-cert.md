# pco web cert

Show the certificate the web interface serves: its mode, the names it is for, until when it
is valid and its SHA-256 fingerprint, which a browser shows to compare. It runs as root.

In mode ca, pco setup's default, the key is the web interface's own and the certificate is
signed by the cluster CA for 90 days; the daemon makes a new one 30 days before it expires,
and when the node's names or addresses or the listen address change.

In the appliance the key is its own and the certificate self-signed for 397 days, made by
the daemon at its first start and again 30 days before it expires: a browser trusts it by
the fingerprint this command prints.

## Usage

```text
pco web cert [flags]
pco web cert [command]
```

## Examples

```text
# The certificate the web interface serves, and its fingerprint
pco web cert

# A new key and certificate of the cluster CA now
pco web cert renew
```

## Commands

- [pco web cert import](pco-web-cert-import.md): Serve a certificate and key of your own in the web interface
- [pco web cert renew](pco-web-cert-renew.md): Make a new key and certificate for the web interface now

## Flags

```text
-h, --help   help for cert
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco web](pco-web.md): Serve the web interface
- [Command reference](index.md): every command of pco, by group
