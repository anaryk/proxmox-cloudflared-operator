# pco web

Serve the web interface over HTTPS, as pco-web.service does. It runs until SIGINT or
SIGTERM. The calls of the page go to the daemon on `--socket`, as those of the other
commands do.

The flags win over the environment, which the unit reads from `/etc/default/pco-web`:

```text
PCO_WEB_LISTEN  the address to listen on (default 127.0.0.1:8643)
PCO_WEB_HOSTS   more host names the interface is reached by, comma separated
PCO_WEB_HSTS    1 to send Strict-Transport-Security, which holds for every port of
                the host name; off by default
```

Without `--cert` and `--key` the certificate and its key are tls.crt and tls.key in
`$CREDENTIALS_DIRECTORY`, where systemd puts the credentials of the unit, and without
`--pin` the certificate pveproxy serves, which its answers to sign-ins must present,
is pveproxy.crt there.

In the appliance users sign in with their user and password of Proxmox VE, at the node's
API as the appliance's daemon reaches it (`--pve-api`, default pve-api.json in
`$CREDENTIALS_DIRECTORY`, which the daemon writes). It listens on net0's IPv4 address only,
as `/etc/pco/net0` says it, which the installer writes and the daemon keeps current,
port 8643 unless PCO_WEB_LISTEN names another, and refuses to start on any other address.
root@pam may not sign in with a password unless PCO_WEB_ALLOW_ROOT=1.

pco-web.service runs it as the user pco-web, whom the daemon answers on its socket while
the unit is installed; by hand, run it as root.

## Usage

```text
pco web [flags]
pco web [command]
```

## Examples

```text
# By hand beside pco-web.service: on another port, with the files the unit reads
pco web --listen 127.0.0.1:8644 --cert /etc/pco/web/tls.crt --key /etc/pco/web/tls.key --pin /etc/pco/web/pveproxy.crt

# On an address of the node, also reached as pco.example.com, with more in the log
pco web --listen 192.0.2.10:8644 --hosts pco.example.com --log-level debug --cert /etc/pco/web/tls.crt --key /etc/pco/web/tls.key --pin /etc/pco/web/pveproxy.crt
```

## Commands

- [pco web cert](pco-web-cert.md): Show the certificate of the web interface

## Flags

```text
    --cert string        certificate file, PEM (default $CREDENTIALS_DIRECTORY/tls.crt)
-h, --help               help for web
    --hosts string       more host names the interface is reached by, comma separated (default $PCO_WEB_HOSTS)
    --key string         key file of the certificate, PEM (default $CREDENTIALS_DIRECTORY/tls.key)
    --listen string      address to listen on (default $PCO_WEB_LISTEN, else 127.0.0.1:8643)
    --log-level string   log level: trace, debug, info, warn or error (default "info")
    --pin string         the certificate pveproxy serves, PEM (default $CREDENTIALS_DIRECTORY/pveproxy.crt)
    --pve-api string     in the appliance, the node's API as its daemon writes it (default $CREDENTIALS_DIRECTORY/pve-api.json)
    --pve-url string     the Proxmox VE API that checks sign-ins (default "https://127.0.0.1:8006/api2/json")
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
