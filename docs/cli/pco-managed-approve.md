# pco managed approve

Consent to a card on the managed network for a guest, named as qemu/101 or lxc/120, in
the identity the daemon sees it in now: a guest re-created under the same VMID, or a
clone, needs a consent of its own. A guest that waits for the managed network is shown
first with why it waits. The daemon refuses the consent when the guest changed since it
was shown, and a guest the last cycle did not see. The card is added in the next cycle,
and goes after the grace once the consent is revoked.

In the appliance a guest whose other card has firewall=1 while its firewall is off waits
with needs-firewall-confirmation: turning its firewall on, which the appliance does for
the card it adds, filters that other card too. `--accept-firewall-impact` confirms that for
this guest; the setting managed.acceptFirewallImpact confirms it for every guest.

It asks the daemon through its socket, which answers only root and pco-web, the user of the
web interface; the exit status is 2 when the daemon could not be asked.

## Usage

```text
pco managed approve <owner> [flags]
```

## Examples

```text
# Consent to a card for lxc/120 in the identity it has now
pco managed approve lxc/120

# The same for a guest whose net0 has firewall=1, in the appliance
pco managed approve lxc/120 --accept-firewall-impact
```

## Flags

```text
    --accept-firewall-impact   confirm that the appliance may turn the firewall of the guest on although another of its cards has firewall=1 (a guest waiting with needs-firewall-confirmation)
-h, --help                     help for approve
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add, network list, candidates, probe and show): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [pco managed](pco-managed.md): Consent to cards on the managed network, and see who has one
- [Command reference](index.md): every command of pco, by group
