# Files

This page lists every path that pco writes or reads on a node: the state and its three
roots, the socket, the units, the files of the web interface and what the package installs.
For each it says what writes it, who may read it, and whether to back it up. It describes
the host profile; the [last section](#the-appliance) lists what differs in the appliance.

Do not edit the state files by hand, except the settings (see [Settings](settings.md)). The
modes in the tables are those pco gives a file it makes. The cluster filesystem sets the
modes and owners of everything below `/etc/pve` by path, and pco never changes them there:
it writes each file through a temporary file of its own in the same directory, so that a
reader sees the old content or the new one and never a part. A temporary file that a crash
left, `.<name>.<random>.tmp`, is removed at the next start of the daemon once it is ten
minutes old.

## The state

The state is a set of small JSON files, one for each object, in three roots.

| Root | Holds | Shared | Owner and mode |
|---|---|---|---|
| `/etc/pve/pco` | State that is not secret: the install, the settings, the writer identity, claims, approvals, manual routes, the node registry. | On the cluster filesystem. | As the cluster filesystem sets. |
| `/etc/pve/priv/pco` | Secrets: the Proxmox token and the Cloudflare credentials. | On the cluster filesystem. | Readable by root only, as everything below `/etc/pve/priv`. |
| `/var/lib/pco` | State of this node: the connectors' files, bindings, the event log, the lock. | No. | `root`, mode `0700`; files `0600` unless a row says otherwise. |

Each object file wraps its data in an envelope with a schema version, a revision and the id
it was stored under:

```json
{
  "schemaVersion": 1,
  "rev": 1,
  "id": "settings",
  "data": {}
}
```

The daemon refuses a file written by a newer schema version and a file that holds the object
of another id. A file larger than 1 MiB is not written, as the cluster filesystem accepts
none. The daemon never creates the shared roots, only `pco setup` does, and it checks that
the cluster filesystem is mounted before it touches them: while `pve-cluster` restarts,
`/etc/pve` is an empty directory of the node's own disk, and what is read from it says
nothing. It tests for the file `/etc/pve/.version`, which exists only while the filesystem is
mounted.

### `/etc/pve/pco`

| Path | Holds | Written by | Owner and mode | Back up |
| --- | --- | --- | --- | --- |
| `meta/install.json` | The install id (12 hexadecimal characters), when it was made, and the profile, `host`. | `pco setup` | as the cluster filesystem sets | Yes; `pco setup --recover` takes the id from Cloudflare when it is lost. |
| `meta/settings.json` | The settings; see [Settings](settings.md). | `pco setup`, `pco apply`, `pco settings apply`, the web interface; by hand | as the cluster filesystem sets | Yes: nothing makes your values again. |
| `meta/leader.json` | The writer identity: install id, generation and a nonce. The tunnel configuration carries it, and it is how pco tells its own writes from those of a stale or a foreign writer. | `pco setup`, `pco setup --recover` | as the cluster filesystem sets | No; a recovery draws a generation above every one in use. |
| `meta/tombstones.json` | DNS records waiting out their grace period. | The daemon | as the cluster filesystem sets | No. |
| `nodes/<node>.json` | The node that runs pco, and its version. | `pco setup` | as the cluster filesystem sets | No; `pco setup` registers the node again. |
| `claims/<hostname>.json` | Who holds a hostname, since when, and who waits. A wildcard is stored as `_wildcard.<name>.json`. | The daemon, `pco claims resolve` | as the cluster filesystem sets | No; the daemon settles claims again from the Notes, and the guest that holds a hostname may then be another. |
| `approvals/<owner>.json` | An approved guest and its identity. A guest `qemu/101` is stored as `qemu_101.json`. | `pco guest approve`, the web interface | as the cluster filesystem sets | Yes: an approval is a decision of an admin. |
| `routes/<id>.json` | Manual routes, which the daemon reads in every cycle. A manual route names an address and not a guest, so its level is `manual`. | `pco route manual add` and `remove`, the web interface | as the cluster filesystem sets | Yes. |
| `segments/<bridge>.json`, `segments/<bridge>.<vlan>.json` | A bridge and VLAN on which routes at `observed` may be served, once an admin acknowledged it. | `pco segment acknowledge` and `revoke` | as the cluster filesystem sets | Yes. |
| `adopted.jsonl` | One line for each DNS record that `pco adopt` replaced, as it was; at most 256 KiB, oldest lines first out. | The daemon, for `pco adopt` | as the cluster filesystem sets | Yes: it is the only copy of those records. |

### `/etc/pve/priv/pco`

| Path | Holds | Written by | Owner and mode | Back up |
| --- | --- | --- | --- | --- |
| `meta/pve-token.json` | The secret of the Proxmox API token of pco. | `pco setup` | root only, as below `/etc/pve/priv` | Only with the care a secret needs; `pco setup --repair` makes the token again. |
| `credentials/<id>.json` | One Cloudflare credential: its label, kind and token, in the clear. | `pco credential add`, `pco setup`, the web interface | root only, as below `/etc/pve/priv` | Only with the care a secret needs; a token can be made again at Cloudflare. |

### `/var/lib/pco`

| Path | Holds | Written by | Owner and mode | Back up |
| --- | --- | --- | --- | --- |
| `manifest.json` | What `pco setup` created, in Proxmox and on the node, in this run and the ones before. `pco uninstall` removes what it lists, and nothing else. | `pco setup` | root, `0600` | Yes, if `pco uninstall` is to know what setup made. |
| `bindings/<hostname>.json` | The address verified for each hostname, with the MAC, the level and, at `port`, where the forwarding table placed the MAC. A file is written when its binding changes, and for the time of the last proof alone only once that moved on by more than 75 seconds: after a restart a proof counts as up to that much older than it is. | The daemon | root, `0600` | No; the addresses are verified again. |
| `meta/engine-memory.json` | What the daemon must still know after a restart: the zones it serves and every zone whose records it ever listed, the tunnels it saw, the guests you confirmed gone, the last check of each credential, and the targets of the egress filter with the tunnel configurations that confirmed them. | The daemon | root, `0600` | No. Removing it forgets the connectors kept for tunnels no credential sees, the zones that left their listing and the guests confirmed gone. |
| `meta/node-addrs.json` | The addresses of the nodes, for the denylist. | The daemon | root, `0600` | No. |
| `meta/soft-deny.json` | The gateways and DNS resolvers of the nodes, and in the appliance of the appliance, each with when it was last seen. A guest whose address is one of them waits for an approval. An address that is not seen for 30 days ages out. | The daemon | root, `0600` | No. |
| `events.log`, `events.log.1` | The event log, one JSON object a line; rotated at 5 MiB, one earlier file is kept. | The daemon | root, `0600` | If you want the history. |
| `tunnels/` | `<tunnel id>.token`, `.env` and `.yml` of each connector, in a directory of mode `0700`; the token and the configuration are `0600`, the env file `0644`. The `.env` file names the install the connector belongs to. A hidden file `.<tunnel id>.pending`, `0600`, says that the files of a tunnel changed and its unit has not been started or restarted since. | The daemon | root; the directory `0700`, `.token` and `.yml` `0600`, `.env` `0644` | No; the daemon reads the token of a tunnel again from Cloudflare. |
| `egress-blocked.json`, `egress-off.json` | The block list and the off switch of the egress filter, `0600`. The off switch is a file that is there while the filter is off. | `pco egress block`, `unblock`, `off` and `on` | root, `0600` | If you keep addresses blocked on purpose. |
| `daemon.lock` | The lock of the node: the daemon holds it for as long as it runs. The file stays where it is, since removing it would let two daemons lock two different files. | The daemon | root, `0600` | No. |

The files of `meta/` and `bindings/` are objects of the same envelope.

## The socket and the lock of the socket

| Path | Holds | Owner and mode |
|---|---|---|
| `/run/pco/` | The runtime directory of `pco.service`. | `0750`; owned by root and the group `pco-web` when the web interface is installed, else the group of root. |
| `/run/pco/pco.sock` | The socket of the daemon, which every `pco` command that asks it uses, and `pco web`. | `0660`, owned the same way. The daemon answers root, and the user `pco-web` when the web interface is installed. |
| `/run/pco/pco.sock.lock` | The lock of the socket, which keeps a second daemon from taking its path. | `0600` |
| `/run/pco/.pco.sock.new/` | Where the daemon binds the socket before it renames it to its place, so that it is reachable all at once and in its final state. A crash may leave it, and the next start removes it. | `0700` |

The directory of the socket must be named `pco`, and its parent must belong to the user the
daemon runs as and not be writable by group or others; a socket elsewhere is chosen with
`--socket`.

## The units

The package installs these files in `/usr/lib/systemd/system`, all mode `0644`:

| Unit | What it is | Owner and mode |
|---|---|---|
| `pco.service` | The daemon. | root, `0644` |
| `pco-egress.service` | Loads the egress filter, `pco egress load`. | root, `0644` |
| `pco-cloudflared@.service` | The template of the connector of one tunnel. | root, `0644` |
| `pco-web.service` | The web interface, `pco web`. | root, `0644` |
| `pco-net.service` | Loads the service-prefix route and filter of the appliance, `pco net load`. Nothing starts it on a host. | root, `0644` |

Their commands, and what each does at a reboot, are in [Operations](operations.md#the-daemon-and-its-units).

What is yours to add is a drop-in. `systemctl edit pco` makes
`/etc/systemd/system/pco.service.d/override.conf`, and the same holds for the other units;
a drop-in that sets `ExecStart=` first clears the command of the package. A connector is
enabled by `systemctl enable --now`, which links
`/etc/systemd/system/multi-user.target.wants/pco-cloudflared@<tunnel id>.service`; the
daemon makes these and `pco uninstall` removes them. The connectors read their credentials
from `/var/lib/pco/tunnels` through `LoadCredential`, so the files can stay unreadable to the
user the connector runs as, `pco-connector`.

The package also installs `/usr/lib/sysusers.d/pco.conf`, which makes the two users the units
run as: `pco-connector` for the connectors, whose processes the egress filter knows by its
uid, and `pco-web` for the web interface.

## The web interface

| Path | Holds | Written by | Owner and mode | Back up |
|---|---|---|---|---|
| `/etc/default/pco-web` | The environment of `pco-web.service`. `pco setup` writes `PCO_WEB_LISTEN=<address>` and keeps any other line you wrote. | `pco setup`; by hand | `0644` | Yes, if you changed it. |
| `/etc/pco/web/` | The credentials of `pco-web.service`. | `pco setup` | `0700`, root only; `/etc/pco` is `0755` |  |
| `/etc/pco/web/tls.crt`, `tls.key` | The certificate the web interface serves and its key. In the mode `ca`, a key of its own and a certificate of 90 days signed by the cluster CA, which the daemon renews 30 days before it ends. In the mode `own`, the pair you gave. In the mode `pveproxy`, links to the pair `pveproxy` serves, which gives away the key of the node: see [Security](security.md). | `pco setup`, `pco web cert renew` and `import`, the daemon | Certificate `0644`, key `0600` | The pair of mode `own`: yours. |
| `/etc/pco/web/pveproxy.crt` | A link to the certificate `pveproxy` serves, which `pco web` pins its connections to the Proxmox API to. | `pco setup`; the daemon points it at the new certificate when `pveproxy` serves another | A link | No. |

The unit loads the three files as the credentials `tls.crt`, `tls.key` and `pveproxy.crt`,
and `pco web` reads them from `$CREDENTIALS_DIRECTORY`, as it starts. The daemon looks at the
files when it starts and then at intervals, renews the certificate of the mode `ca`, makes the
links follow `pveproxy`, and restarts `pco-web.service` when what it loaded is not what the
files hold now; the renewal is an event of the kind `web`. It reads the files of the cluster CA,
`/etc/pve/pve-root-ca.pem` and, to sign a certificate, `/etc/pve/priv/pve-root-ca.key`, only
in `pco setup` and in the daemon, never in `pco web`.

## What the package installs

| Path | What it is |
|---|---|
| `/usr/bin/pco` | The binary. |
| `/usr/share/pco/release-key.gpg` | The key `pco upgrade` checks a release with. |
| `/usr/share/pco/cloudflared-versions.json` | The versions of `cloudflared` this release vetted. |
| `/usr/share/man/man1/pco.1.gz`, `pco-*.1.gz` | A man page for each command, such as `man pco-route-manual-add`, with the text of the [command reference](cli/index.md). |
| `/usr/share/bash-completion/completions/pco`, `/usr/share/zsh/vendor-completions/_pco`, `/usr/share/fish/vendor_completions.d/pco.fish` | The completion of `pco` in bash, zsh and fish, which a shell started after the install loads; `pco completion` prints the same scripts. |
| `/usr/share/doc/pco/README.md`, `copyright`, `web-licenses.txt` | The readme, the licence and the notices of the packages the web interface carries. |
| `/usr/share/doc/pco/docs/` | These pages. |

`pco setup` adds, when it installs `cloudflared` itself, the apt source
`/etc/apt/sources.list.d/cloudflared.sources` and the key
`/usr/share/keyrings/cloudflare-main.gpg`, and the manifest lists both. [Uninstall](uninstall.md)
says when `pco uninstall` removes them.

## What pco reads and does not write

| Path | Why |
|---|---|
| `/etc/pve/.version` | Whether the cluster filesystem is mounted. |
| `/etc/pve/local/pveproxy-ssl.pem`, else `/etc/pve/local/pve-ssl.pem`, and the key beside it | The certificate `pveproxy` serves, which `pco web` pins and the mode `pveproxy` links to. |
| `/etc/pve/pve-root-ca.pem`, `/etc/pve/priv/pve-root-ca.key` | The cluster CA, to sign the certificate of the web interface. |
| `/etc/resolv.conf` | The resolvers of the node, which the egress filter lets the connectors reach on port 53. |
| `/etc/hosts` | The fully qualified name of the node, for the certificate and the host names of the web interface. |
| `/etc/localtime` | The time zone the web interface shows the node's times in, unless `TZ` is set. |
| `/usr/bin/cloudflared`, `/usr/sbin/nft`, `/usr/bin/systemctl`, `/usr/bin/journalctl` | The programs it runs. |

The logs are not files of pco: the daemon and the connectors write to the journal, `journalctl
-u pco` and `journalctl -u pco-cloudflared@<tunnel id>`. The nftables table `inet pco_egress`
is in the kernel and not on disk; `pco egress show` shows it.

## The appliance

The appliance is pco in a container of its own, and it keeps its state on a volume instead
of the cluster filesystem. The differences in the paths:

| Path | What it is |
|---|---|
| `/etc/pco/profile` | A file that says `appliance`. Without it, or with `host`, the machine is a host; any other content is an error and never the host. |
| `/var/lib/pco` | The mount point of the state volume. The three roots are below it: `cluster` and `private` for what the host keeps in `/etc/pve/pco` and `/etc/pve/priv/pco`, and `/var/lib/pco` itself for the state of the node. Writes are flushed to disk. |
| `/var/lib/pco/.volume` | The marker the installer puts on the state volume. Without it the volume is not mounted, or is a new one: the daemon exits with status 78 and stays stopped instead of starting again every 5 seconds, and `pco appliance repair` on the node is what helps. |
| `/var/lib/pco/bootstrap.json` | What the installer pushes into the container, `0600`, secrets in the clear. `pco appliance init` removes it as soon as it has read it, and writes the store from it. |
| `/var/lib/pco/pve-ca.pem` | The CA the appliance verifies the Proxmox API with. |
| `/var/lib/pco/manifest.json` | What the installer made on the node for this appliance, which the installer reads back as untrusted input. |
| `/var/lib/pco/upgrades/` | Where `pco upgrade` works, with the package of the version before an upgrade kept in `previous/`. |
| `/run/pco-appliance/identity-ok` | A file on a tmpfs that the daemon writes when it has proved in this boot that the container is the appliance and not a copy of it, and removes on a copy. The connectors start only while it is there. |
| `/etc/pco/net0` | The IPv4 address of net0, the appliance's management card, which `pco-web` listens on and nowhere else. `pco appliance install` writes it, `pco appliance repair` when it is missing, and the daemon writes the card's address when it changes, as a new lease of DHCP does, and restarts `pco-web`. |
| `/etc/default/pco-web` | The environment of `pco-web`. The installer writes it without `PCO_WEB_LISTEN`, so that `pco-web` listens on net0's address, port 8643; `PCO_WEB_LISTEN` may name another port of that address and nothing else. `PCO_WEB_ALLOW_ROOT=1` lets `root@pam` sign in with a password. |
| `/etc/pco/web/tls.crt`, `tls.key`, `mode` | A key of the appliance's own and a self-signed certificate of 397 days, which the daemon makes at its first start and again 30 days before it ends, or a pair you imported (`mode` says `own`), which it leaves alone. A new address of net0 keeps the certificate, and with it the fingerprint the browser trusts. |
| `/etc/pco/web/pve-api.json` | The Proxmox API as the daemon reaches it: its address, the name its certificate is verified under and the CA, which `pco-web` signs users in at. The daemon writes it; nothing in it is secret. |

On the node, `pco appliance install` keeps a journal of each run in
`/root/.pco-appliance-install/<run>.json` (`0600`) and the lock of the installer in the same
directory, and stages the bootstrap in `/run/pco-appliance-install-<run>/` while it pushes
it. The journal never holds a secret. The drop-ins `pco.service.d/appliance.conf` and
`pco-cloudflared@.service.d/appliance.conf` in `/etc/systemd/system` of the appliance make
`pco-net.service` a requirement of both, and the second adds the condition on the identity
file.

## What to back up

The state that cannot be made again is what an admin decided: the settings, the approvals,
the manual routes, the acknowledged segments, the snapshots in `adopted.jsonl`, the
credentials and the manifest. On a host the settings, approvals, manual routes, segments and
snapshots are in `/etc/pve/pco`, which a backup of the cluster filesystem has, and the
credentials are in `/etc/pve/priv/pco`: a backup that holds them is a secret too. The
manifest is in `/var/lib/pco`. Everything else is made again by the daemon: bindings are
verified again, claims settled again from the Notes, tunnel tokens read again from
Cloudflare.

If the whole store is lost, `pco setup --recover` takes the install and its writer
generation from the tunnels the Cloudflare token sees, and starts observe-only, with the
settings that are there or else the defaults.
