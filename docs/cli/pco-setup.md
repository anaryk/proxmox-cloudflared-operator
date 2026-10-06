# pco setup

Prepare this Proxmox VE node for pco: role PCO, user pco@pve and its API token, the gate
tags as registered tags, the cloudflared package, a first Cloudflare token, the identity of
the install and the daemon. Every step looks first: running setup again finishes a run that
stopped, and changes nothing on a node that is set up. What setup creates is noted in the
manifest that pco uninstall follows. It runs as root on the node.

The Cloudflare token is read from `--cf-token-file`, from standard input with `--cf-token-stdin`,
or asked for without being shown; never from an argument or the environment. It is stored
only when it can do what pco needs, and only while the daemon is stopped.

`--repair` re-asserts the role, the user, the token and the tags, as after a restore of the
node. `--recover` adopts the install whose tunnels the Cloudflare token sees, after its store
was lost; when the token sees several, `--install-id` chooses.

A node whose store holds no install but that runs connectors of one is refused, as a new
install would never prune them: pco setup `--recover` adopts that install, and pco uninstall
`--keep-cloudflare` removes pco from the node so that setup can start over. `--new-install`
starts a new install beside them regardless.

The web interface, pco-web.service, listens on the node's address in the cluster status,
port 8643, or on `--web-listen`; `--no-web` leaves it out. Its certificate, by `--web-cert`:

```text
ca        a key of its own and a certificate signed by the cluster CA for 90 days, which
          the daemon renews; browsers that trust the cluster CA, as for port 8006, take it
          (the default)
own       the certificate and key of --web-cert-file and --web-key-file, copied in
pveproxy  the certificate and key pveproxy serves: the web interface then holds
          pveproxy's own key
```

`--repair` keeps the mode chosen before unless `--web-cert` says otherwise.

## Usage

```text
pco setup [flags]
```

## Examples

```text
# Set up the node, asking for what is missing
pco setup

# Without a question, with the Cloudflare token of a file
pco setup --yes --cf-token-file /root/cf-token

# Put the role, the user, the token and the tags back after a restore of the node
pco setup --repair

# Adopt the install the token sees, after the store was lost
pco setup --recover --cf-token-file /root/cf-token

# The web interface with a certificate and key of your own
pco setup --repair --web-cert own --web-cert-file /root/pco.example.com.crt --web-key-file /root/pco.example.com.key
```

## Flags

```text
    --cf-token-file string   read the Cloudflare API token from this file
    --cf-token-stdin         read the Cloudflare API token from standard input, which must not be a terminal
-h, --help                   help for setup
    --install-id string      with --recover: the install to adopt, when the token sees several
    --new-install            start a new install on a node that runs connectors of another install: the connectors of the other install keep running and are never pruned by the new one; pco status reports them
    --no-registered-tags     do not register the gate tags, which lets whoever may edit a guest set them
    --no-web                 do not set up the web interface
    --recover                adopt the install whose tunnels the Cloudflare token sees, after the store was lost
    --repair                 re-assert the role, the user, the token and the tags in Proxmox, and start the daemon
    --skip-cloudflared       do not install cloudflared when it is missing
    --verbose                name every zone the Cloudflare token leaves out, not only those this install served
    --web-cert string        the certificate of the web interface: ca (the default), own or pveproxy
    --web-cert-file string   with --web-cert own: the certificate, PEM
    --web-key-file string    with --web-cert own: the key of the certificate, PEM
    --web-listen string      the address the web interface listens on, with or without a port (default: the node's address in the cluster status, port 8643)
-y, --yes                    take the default answers and ask nothing (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
