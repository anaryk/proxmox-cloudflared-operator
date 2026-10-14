# pco appliance convert

Make a combined appliance the controller of a cluster, in place, so that the agent
containers of the other nodes can join it. Its store, its credentials and its routes stay, and
its connectors run on throughout: it goes on serving the node it runs on.

Run on the node, as root, with `--vmid`: it checks that the container is a combined appliance
of pco appliance install and that no other controller serves the cluster, runs pco appliance
convert inside it, which records the kind and restarts the daemon (it then makes the install's
CA and serves the agent endpoint, tcp/8644 on net0's address), tags the container a
controller, opens that port in its firewall to ipset pco-agents alone, and lets its token
read its own storage. `--ha` makes it a Proxmox HA resource as pco appliance install `--ha` does;
read that command's help first. A convert that failed is run again to finish.

Then make a join token for each other node on the controller, and install an agent there
with it.

## Usage

```text
pco appliance convert [flags]
```

## Examples

```text
# On the node of the combined appliance in container 120
pco appliance convert --vmid 120

# The same, and make it an HA resource kept on two nodes
pco appliance convert --vmid 120 --ha --ha-nodes pve1,pve2

# Then a join token for node pve2, made on the controller
pct exec 120 -- pco cluster join-token --node pve2 --file /root/pve2.join

# And on pve2, with the token copied there
pco appliance install --kind agent --storage local-zfs --join-file /root/pve2.join --checksums checksums.txt
```

## Flags

```text
    --ha                 make the controller a Proxmox HA resource
    --ha-nodes strings   the nodes the HA controller is kept on, repeated or separated by commas
-h, --help               help for convert
    --vmid int           the VMID of the combined appliance
-y, --yes                ask nothing (needed without a terminal)
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
