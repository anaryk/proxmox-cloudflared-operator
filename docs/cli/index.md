# Command reference

The commands of `pco`, by group, as `pco help` shows them: each page holds the help of the
command, its examples and its flags.

Cloudflare Tunnel operator for Proxmox VE. pco publishes the hostnames that tagged guests
list in their Notes through Cloudflare Tunnels: it keeps the DNS records, the tunnels and a
cloudflared connector for each tunnel in line with the Notes. pco daemon does that work.
Most other commands ask it through its socket, which answers only root and pco-web, the user
of the web interface; the others work on the machine directly, and the help of each says who
may run it.

Exit status:

```text
0  all is well
1  the command ran and found something to look at: problems in the state, a failed
   doctor check or diagnosis step, a request the daemon refused; or it failed otherwise
2  it could not ask the daemon: the daemon is not running, its socket refused the
   connection, no answer came in time, or the answer could not be read
```

## Usage

```text
pco [command]
```

## Examples

```text
# What pco found and did on this node
pco status

# The help of a command
pco help route manual add
```

## Getting started

- [pco apply](pco-apply.md): Leave observe-only mode and start publishing
- [pco credential](pco-credential.md): Manage the Cloudflare tokens of the daemon
  - [pco credential add](pco-credential-add.md): Check a Cloudflare API token and store it
  - [pco credential check](pco-credential-check.md): Check what the token of a credential can do
  - [pco credential list](pco-credential-list.md): List the stored credentials
  - [pco credential remove](pco-credential-remove.md): Remove a credential, when nothing is left that it manages
- [pco setup](pco-setup.md): Prepare this Proxmox VE node for pco, repair it, or recover a lost store
- [pco status](pco-status.md): Show what the daemon found and did

## Routes and guests

- [pco adopt](pco-adopt.md): Take over the DNS record that stands in the way of a hostname
- [pco claims](pco-claims.md): Show who holds each public hostname, and hand one to another owner
  - [pco claims list](pco-claims-list.md): List the claims on the public hostnames
  - [pco claims resolve](pco-claims-resolve.md): Hand a public hostname to another owner that claims it
- [pco diagnose](pco-diagnose.md): Walk the chain of the route of a hostname
- [pco guest](pco-guest.md): Approve guests for publishing, for admission mode approve
  - [pco guest approve](pco-guest-approve.md): Approve a guest in the identity it has now
  - [pco guest list](pco-guest-list.md): List the approved guests, the guests that wait for approval and the tagged guests
  - [pco guest revoke](pco-guest-revoke.md): Remove the approval of a guest
- [pco network](pco-network.md): Attach pco to the networks of guests the node has no address in
  - [pco network attach](pco-network-attach.md): Attach a network: a leg of pco's own into a bridge or VLAN
  - [pco network candidates](pco-network-candidates.md): List the bridges and VLANs of the node a network may be attached to
  - [pco network detach](pco-network-detach.md): Detach a network: remove its leg and its service addresses
  - [pco network list](pco-network-list.md): List the networks attached, with their legs and the routes served through them
  - [pco network probe](pco-network-probe.md): Ask who answers for an address on a bridge or VLAN
  - [pco network show](pco-network-show.md): Show a network with its leg, its service addresses and the routes served through it
- [pco plan](pco-plan.md): Show what the daemon would change and what stands in its way
- [pco route](pco-route.md): Make and remove the routes an admin writes by hand
  - [pco route manual](pco-route-manual.md): Manual routes: hostnames published to a guest or an address without its Notes
    - [pco route manual add](pco-route-manual-add.md): Make a manual route
    - [pco route manual list](pco-route-manual-list.md): List the manual routes
    - [pco route manual remove](pco-route-manual-remove.md): Remove a manual route
- [pco routes](pco-routes.md): List the routes and how each fares
- [pco segment](pco-segment.md): Acknowledge the bridges and VLANs routes at observed may be served on
  - [pco segment acknowledge](pco-segment-acknowledge.md): Serve the routes at observed on a segment
  - [pco segment list](pco-segment-list.md): List the segments routes at observed were proven on, and those acknowledged
  - [pco segment revoke](pco-segment-revoke.md): Take the acknowledgement of a segment back

## Operating

- [pco daemon](pco-daemon.md): Run the daemon on this node
- [pco doctor](pco-doctor.md): Check the installation
- [pco egress](pco-egress.md): Show and control the filter that confines the connectors
  - [pco egress block](pco-egress-block.md): Keep the connectors from an address, whatever the daemon verified
  - [pco egress off](pco-egress-off.md): Switch the egress filter off, when it is itself the fault
  - [pco egress on](pco-egress-on.md): Switch the egress filter back on
  - [pco egress show](pco-egress-show.md): Show whether the egress filter is on, what its sets hold and what it rejected
  - [pco egress unblock](pco-egress-unblock.md): Take an address off the block list of this node
- [pco events](pco-events.md): List what changed, as the daemon keeps it
- [pco export](pco-export.md): Write the install to an encrypted bundle, the backup to keep
- [pco import](pco-import.md): Restore the install of an encrypted bundle on this node
- [pco net](pco-net.md): Show how the appliance keeps the service prefix to itself
  - [pco net show](pco-net-show.md): Show the device, route, rules and table that keep the service prefix in the appliance
- [pco netns](pco-netns.md): Show the gateway namespace the attached networks are reached through
  - [pco netns status](pco-netns-status.md): Show the legs of the gateway namespace and whether it is as pco left it
- [pco settings](pco-settings.md): Show the settings and save them
  - [pco settings apply](pco-settings-apply.md): Save the settings of a file, at the revision they were read at
  - [pco settings show](pco-settings-show.md): Show the settings with their revision
- [pco sync](pco-sync.md): Ask the daemon for a reconcile cycle now
- [pco tunnel](pco-tunnel.md): Act on the tunnels of the install
  - [pco tunnel rotate](pco-tunnel-rotate.md): Give a tunnel a new secret, so that no connector but pco's runs it
- [pco upgrade](pco-upgrade.md): Upgrade pco and cloudflared in the appliance from signed releases
- [pco web](pco-web.md): Serve the web interface
  - [pco web cert](pco-web-cert.md): Show the certificate of the web interface
    - [pco web cert import](pco-web-cert-import.md): Serve a certificate and key of your own in the web interface
    - [pco web cert renew](pco-web-cert-renew.md): Make a new key and certificate for the web interface now

## Lifecycle

- [pco appliance](pco-appliance.md): Commands of the pco appliance
  - [pco appliance grant-network](pco-appliance-grant-network.md): Grant the appliance a bridge or vnet to attach cards to
  - [pco appliance init](pco-appliance-init.md): Make the store of this appliance from the bootstrap the installer pushed
  - [pco appliance install](pco-appliance-install.md): Install the pco appliance on this node
  - [pco appliance managed](pco-appliance-managed.md): Make the managed network of the appliance
    - [pco appliance managed enable](pco-appliance-managed-enable.md): Make the SDN vnet of the managed network and grant it to the appliance
  - [pco appliance network](pco-appliance-network.md): Grant the appliance a bridge or vnet, in short
    - [pco appliance network allow](pco-appliance-network-allow.md): Grant the appliance a bridge or vnet, or one VLAN of it
  - [pco appliance recover](pco-appliance-recover.md): Draw a writer epoch above the last write at Cloudflare, after a rollback or a restore
  - [pco appliance repair](pco-appliance-repair.md): Repair the appliance after a restore or a changed certificate
  - [pco appliance revoke-network](pco-appliance-revoke-network.md): Take back the grant of a bridge or vnet from the appliance
  - [pco appliance uninstall](pco-appliance-uninstall.md): Remove the appliance from this node
- [pco completion](pco-completion.md): Print the script that completes pco in a shell
- [pco uninstall](pco-uninstall.md): Remove pco from this node
- [pco version](pco-version.md): Print the build version

## Flags

```text
-h, --help            help for pco
    --json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
    --socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```
