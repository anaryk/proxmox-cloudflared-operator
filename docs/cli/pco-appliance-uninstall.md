# pco appliance uninstall

Remove the appliance and what the installer made for it from this node, as the marks
of the objects and the manifest in the appliance name them: the container with its state
volume, its token, its network grants, the NoAccess lines the installer added while they
are as it made them, and user pco@pve, role PCO and pool pco when nothing else uses them,
the gate tags the installer registered, and with `--keep-template=false` the template it
downloaded for the appliance, which the manifest names. It lists what goes and what
stays, and why, and asks once; `--yes` answers that.

What the install has at Cloudflare is deleted through the running appliance with
`--purge-cloudflare`, left with `--keep-cloudflare`, or asked about; with `--yes`, an install
with something at Cloudflare needs one of the two, as the credentials that reach it go
with the container. A copy of the appliance beside its original leaves Cloudflare alone,
as what it reaches there is the original's.

A part that fails is reported and the rest goes on; running it again finishes it. It
runs as root on the node that has the container; on another node of the cluster it
refuses and names that node, and only a container no node has counts as gone, whose
leftovers it removes. It exits 0 once the appliance is removed, 1 otherwise.

## Usage

```text
pco appliance uninstall [flags]
```

## Examples

```text
# Remove the appliance, asking about everything
pco appliance uninstall --vmid 120

# Without a question, deleting its DNS records and tunnel at Cloudflare
pco appliance uninstall --vmid 120 --yes --purge-cloudflare

# Without a question, leaving Cloudflare as it is, and removing the template too
pco appliance uninstall --vmid 120 --yes --keep-cloudflare --keep-template=false
```

## Flags

```text
-h, --help               help for uninstall
    --keep-cloudflare    leave the DNS records and the tunnel of the install at Cloudflare as they are
    --keep-template      keep the template the installer downloaded for the appliance; another template of pco always stays (default true)
    --purge-cloudflare   delete the DNS records and the tunnel of the install at Cloudflare, through the appliance
    --vmid int           the VMID of the appliance
-y, --yes                remove the appliance without asking (needed without a terminal); with something at Cloudflare, --purge-cloudflare or --keep-cloudflare as well
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
