# pco web cert renew

Make a new key and certificate signed by the cluster CA for the web interface now, and
restart pco-web.service so that it serves them, as after a suspected leak of its key.
Only a certificate of mode ca is renewed. In the appliance it makes a new key and
self-signed certificate, also in place of one of your own. It runs as root.

## Usage

```text
pco web cert renew [flags]
```

## Examples

```text
# A new key and certificate of the cluster CA now
pco web cert renew

# And the fingerprint to compare in the browser
pco web cert
```

## Flags

```text
-h, --help   help for renew
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco web cert](pco-web-cert.md): Show the certificate of the web interface
- [Command reference](index.md): every command of pco, by group
