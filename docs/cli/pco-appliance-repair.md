# pco appliance repair

Put the appliance right on this node after a restore, after the certificate of the API
changed, or when its token was lost. It starts the container if it is stopped and stops it
again at the end. Its state volume decides how:

```text
with the state of an install, the API token is made anew, the gate tags and the
certificate are checked again (--api-ca for a certificate of a private CA), and pco
appliance init repairs the store with the token, the MACs, the endpoint and the
node's addresses as they are now;
without state, only with --recover (a restore to another VMID, a restore over the
appliance, a lost volume): the container gets a state volume again if it has none, the
volume is marked, and pco appliance init adopts the install the Cloudflare token sees
(--install-id when it sees several), with the manifest rebuilt from what pco made in
Proxmox.
```

A copy of the appliance beside the one it was made from, a clone or a restore while the
original is still there, is refused: it would write the install of the original with its
credentials. Remove the copy, or install an appliance anew.

A repair takes nothing back when it fails; running it again finishes it. It runs as root
on the node that has the container; on another node of the cluster it refuses and names
that node. It exits 0 once the appliance is repaired, 1 otherwise.

## Usage

```text
pco appliance repair [flags]
```

## Examples

```text
# After the cluster CA was made anew
pco appliance repair --vmid 120

# The API now presents a certificate of a private CA
pco appliance repair --vmid 120 --api-ca /root/example-ca.pem

# A restore to VMID 121 with an empty state volume
pco appliance repair --vmid 121 --recover --cf-token-file /root/cf-token
```

## Flags

```text
    --api-ca string          a CA file for an API certificate that neither the cluster CA nor the system roots verify
    --api-host string        the node's address the appliance reaches the API at (default: its address on the bridge)
    --cf-token-file string   read a Cloudflare API token from this file
    --gate-tag string        the gate tag of the appliance (default cf-tunnel)
-h, --help                   help for repair
    --install-id string      with --recover: the install to adopt, when the token sees several
    --no-registered-tags     do not register the gate tags, which lets whoever may edit a guest set them
    --recover                the volume holds no state: adopt the install the Cloudflare token sees
    --state-size int         the size of the state volume, in GiB (default 1)
    --storage string         the storage of the container and its state volume (default: the one storage that holds containers)
    --vmid int               the VMID of the appliance
-y, --yes                    take the default answers and ask nothing (needed without a terminal); never adds NoAccess lines
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco appliance](pco-appliance.md): Commands of the pco appliance
- [Command reference](index.md): every command of pco, by group
