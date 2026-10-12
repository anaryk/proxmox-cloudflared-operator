# pco uninstall

Remove pco from this node: the daemon, the connectors, the egress filter, what setup created
in Proxmox as its manifest lists, and the store. It looks first, lists what is there and asks
once; `--yes` answers that. Deleting the DNS records and the tunnel of the install at
Cloudflare is asked on its own, or done with `--purge-cloudflare` and left with
`--keep-cloudflare`; with `--yes`, an install with Cloudflare credentials needs one of the two,
as nothing on this node can remove those objects once the store is gone. Removing the
cloudflared package setup installed is asked as well, or done with `--remove-cloudflared`;
with `--yes` it stays without that flag. A part that fails is
reported and the rest goes on; the store is then kept, and running pco uninstall again
finishes the rest. It runs as root on the node.

The SDN zone pco and vnet pconet of the managed network go last, under the lock of the SDN
configuration, and only once no guest of the cluster has a card on pconet: the apply takes
the bridge away from every node. `--detach-managed` removes the cards pco attached right after
the daemon stopped and waits, bounded, for the configurations to drop them; a VM without
network hotplug drops its card when it restarts.

On a node of a cluster that other nodes keep the install running on, the node leaves it:
it hands the lease off when it holds it, stops and removes its connectors, the egress filter,
the web interface, its address in the firewall ipset pco-nodes and its local state, and its
entry of the node registry becomes removed, so that its certificate is refused. What the
nodes share stays: the store, the credentials, what is at Cloudflare and in Proxmox, and
the firewall rule, which the last node removes. A node that holds the lease and cannot hand
it off, and one whose connectors may be the only ones serving the shared tunnel, are
refused unless `--force` says to remove them regardless. Drain the node first with
pco node drain.

## Usage

```text
pco uninstall [flags]
```

## Examples

```text
# Remove pco, asking about everything
pco uninstall

# Without a question, deleting the DNS records and the tunnel and removing cloudflared
pco uninstall --yes --purge-cloudflare --remove-cloudflared

# Without a question, leaving Cloudflare as it is and taking pco's cards off the guests
pco uninstall --yes --keep-cloudflare --detach-managed

# Take a node out of a cluster that keeps running the install, even while it holds the lease
pco uninstall --yes --force
```

## Flags

```text
    --detach-managed       remove the cards pco attached on the managed network first, so that its zone and vnet can go
    --force                take a node out of a cluster although it holds the lease and cannot hand it off, or no other node is active
-h, --help                 help for uninstall
    --keep-cloudflare      leave the DNS records and the tunnel of the install at Cloudflare as they are, without looking at them
    --purge-cloudflare     delete the DNS records and the tunnel of the install at Cloudflare
    --remove-cloudflared   remove the cloudflared package and its apt source, when setup installed them
-y, --yes                  remove pco without asking (needed without a terminal); an install with Cloudflare credentials needs --purge-cloudflare or --keep-cloudflare as well, and cloudflared stays without --remove-cloudflared
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
