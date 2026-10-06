# pco appliance

Commands of the pco appliance, the container that runs pco and its connectors on a
Proxmox VE node in place of the host install. Of its commands, install, repair, uninstall,
grant-network and revoke-network run as root on the node, and init and recover as root
inside the appliance.

## Usage

```text
pco appliance [flags]
pco appliance [command]
```

## Examples

```text
# On the node: install the appliance
pco appliance install --storage local-zfs --checksums checksums.txt

# Inside the appliance, after a rollback to a snapshot
pco appliance recover
```

## Commands

- [pco appliance grant-network](pco-appliance-grant-network.md): Grant the appliance a bridge or vnet to attach cards to
- [pco appliance init](pco-appliance-init.md): Make the store of this appliance from the bootstrap the installer pushed
- [pco appliance install](pco-appliance-install.md): Install the pco appliance on this node
- [pco appliance recover](pco-appliance-recover.md): Draw a writer epoch above the last write at Cloudflare, after a rollback or a restore
- [pco appliance repair](pco-appliance-repair.md): Repair the appliance after a restore or a changed certificate
- [pco appliance revoke-network](pco-appliance-revoke-network.md): Take back the grant of a bridge or vnet from the appliance
- [pco appliance uninstall](pco-appliance-uninstall.md): Remove the appliance from this node

## Flags

```text
-h, --help   help for appliance
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
