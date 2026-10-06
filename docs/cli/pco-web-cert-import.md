# pco web cert import

Check a certificate and its key, both PEM, and put them in place of the ones the web
interface serves, which makes its mode own, then restart pco-web.service. The key must be
the certificate's, the certificate valid now and for one of the node's names or addresses
(the appliance's, in the appliance). pco does not renew it; pco doctor warns 30 days before
it expires. It runs as root.

## Usage

```text
pco web cert import <crt> <key> [flags]
```

## Examples

```text
# Serve the certificate and key of pco.example.com
pco web cert import /root/pco.example.com.crt /root/pco.example.com.key

# Back to a certificate of the cluster CA, renewed by pco
pco setup --repair --web-cert ca
```

## Flags

```text
-h, --help   help for import
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco web cert](pco-web-cert.md): Show the certificate of the web interface
- [Command reference](index.md): every command of pco, by group
