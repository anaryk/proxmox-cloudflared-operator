# pco daemon

Run the daemon: it reads the guests from Proxmox, keeps the tunnels, the connectors and
the DNS records at Cloudflare in line with their notes, and answers the commands of
pco on its socket. It runs until SIGINT or SIGTERM.

The directories of the store are those of the profile: of a Proxmox node, or of the
appliance when `/etc/pco/profile` says appliance. The flags move them for a test
or an unusual install, all three or none: a store of its own, without the check for the
cluster filesystem. Moving only some of them would mix it with the tokens, the connectors
and the lock of the node.

In a terminal the log is written as lines to read, with control characters replaced;
otherwise, as for the journal, as JSON lines.

pco.service runs it as root, and a second daemon on the same node refuses to start. In the
appliance it exits with 78 when its state volume is not mounted with its marker, and the
unit does not start it again then.

## Usage

```text
pco daemon [flags]
```

## Examples

```text
# As pco.service runs it
pco daemon

# With more in the log, in a drop-in of pco.service (systemctl edit pco)
pco daemon --log-level debug
```

## Flags

```text
    --cluster-dir string   directory of the state the cluster shares; only with the two others (default /etc/pve/pco)
-h, --help                 help for daemon
    --local-dir string     directory of the state of this node and of the lock of the daemon; only with the two others (default /var/lib/pco)
    --log-level string     log level: trace, debug, info, warn or error (default "info")
    --node string          name of this node in Proxmox (default: the host name up to its first dot); not taken in the appliance
    --private-dir string   directory of the secrets the cluster shares; only with the two others (default /etc/pve/priv/pco)
    --profile string       run as host or appliance, for a test (default: what /etc/pco/profile says, host without it)
    --pve-ca-file string   CA bundle to verify the Proxmox API with; not needed for a loopback URL
    --pve-url string       base URL of the Proxmox API (default "https://127.0.0.1:8006")
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
