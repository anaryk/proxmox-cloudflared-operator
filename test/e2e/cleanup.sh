#!/bin/sh
# Removes from a Proxmox VE node what a run of the end-to-end suite leaves
# behind, also after a run that was cut off. It acts only on what the suite
# recorded as its own in /var/lib/pco-e2e: the pco install it set up (purged
# at Cloudflare when the run was against the real API, its managed network
# detached), its guests 9100 to 9199 and 9350 to 9399 named e2e-*, the
# drop-in of pco.service, the address of the node on vmbr1, the tables of
# others it flushed, the registered tags, kernel parameters and firewall
# service the networking scenarios changed, their runtime bridges, and then
# the pco package and the user of the connectors. pveproxy, which the suite
# stops for a while, is started when it is not running. It runs as root on
# the node and can be run again.
set -u

state=/var/lib/pco-e2e
owned=$state/owned
bridge=vmbr1
address=10.77.0.1/24
dropin=/etc/systemd/system/pco.service.d/e2e.conf
installfile=/etc/pve/pco/meta/install.json
failed=0

warn() {
	echo "cleanup: $*" >&2
	failed=1
}

if [ "$(id -u)" -ne 0 ]; then
	echo "cleanup: run as root" >&2
	exit 2
fi

if ! systemctl is-active --quiet pveproxy; then
	if systemctl start pveproxy; then
		echo "cleanup: started pveproxy again"
	else
		warn "starting pveproxy failed"
	fi
fi

installed=
if [ -e "$installfile" ]; then
	installed=$(perl -MJSON::PP -e 'local $/; my $d = decode_json(<STDIN>); print $d->{data}{id} // ""' <"$installfile")
fi

if [ ! -d "$state" ]; then
	if [ -e "$installfile" ]; then
		warn "pco is set up here as install ${installed:-(unreadable)}, and no run of the suite recorded anything" \
			"in $state: everything is left alone"
	else
		echo "cleanup: $state is missing: no run of the suite left anything to remove here"
	fi
	exit "$failed"
fi
rm -f "$state/cf-token"

# S14 flushes the ruleset after saving the tables that are not pco's; those
# that did not come back are loaded as they were.
for saved in "$state"/tables/table-*.nft; do
	[ -e "$saved" ] || continue
	read -r _ family name _ <"$saved"
	if nft list table "$family" "$name" >/dev/null 2>&1; then
		rm -f "$saved"
	elif nft -f "$saved"; then
		echo "cleanup: loaded the table $family $name again"
		rm -f "$saved"
	else
		warn "loading the table $family $name again failed; it is saved in $saved"
	fi
done
rmdir "$state/tables" 2>/dev/null || true

# pco is the suite's when the install set up here is the one the suite
# recorded, or when none is set up and the suite ran here.
ours=$(sed -n 's/^install=//p' "$owned" 2>/dev/null | head -n 1)
mode=$(sed -n 's/^mode=//p' "$owned" 2>/dev/null | head -n 1)
pco=no
if [ -n "$installed" ] && [ "$installed" = "$ours" ]; then
	pco=ours
elif [ -e "$installfile" ]; then
	warn "pco is set up here as install ${installed:-(unreadable)}, which no run of the suite recorded as its own" \
		"($owned names ${ours:-none}): pco, its package, its connectors and its egress table are left alone"
elif [ -e "$owned" ]; then
	pco=ours
fi

# The registered tags go back to what they were before the networking
# scenarios added to them, before the uninstall takes away the tag setup
# registered.
if [ -e "$state/registered-tags" ]; then
	read -r tags <"$state/registered-tags"
	if [ -z "$tags" ]; then
		if pvesh set /cluster/options --delete registered-tags >/dev/null; then
			rm -f "$state/registered-tags"
		else
			warn "deleting the registered tags failed"
		fi
	elif pvesh set /cluster/options --registered-tags "$tags" >/dev/null; then
		rm -f "$state/registered-tags"
	else
		warn "putting the registered tags back to '$tags' failed"
	fi
fi

# The guests of the networking scenarios go before the uninstall: a card left
# on pconet would keep the SDN zone of the managed network. The daemon is
# stopped first, so that it does not act on their going.
destroy_guest() {
	id=$1
	if [ -e "/etc/pve/lxc/$id.conf" ]; then
		if ! grep -q '^hostname: e2e-' "/etc/pve/lxc/$id.conf"; then
			echo "cleanup: container $id is not one of the suite's; left alone"
			return
		fi
		pct unlock "$id" >/dev/null 2>&1 || true
		if pct status "$id" | grep -q running; then
			pct stop "$id" || warn "stopping container $id failed"
		fi
		for snap in $(pct listsnapshot "$id" 2>/dev/null | awk '{ print $2 }' | grep -v '^current$'); do
			pct delsnapshot "$id" "$snap" --force 1 || warn "deleting snapshot $snap of $id failed"
		done
		pct destroy "$id" --purge 1 || warn "destroying container $id failed"
	elif [ -e "/etc/pve/qemu-server/$id.conf" ]; then
		if ! grep -q '^name: e2e-' "/etc/pve/qemu-server/$id.conf"; then
			echo "cleanup: VM $id is not one of the suite's; left alone"
			return
		fi
		qm unlock "$id" >/dev/null 2>&1 || true
		if qm status "$id" | grep -q running; then
			qm stop "$id" || warn "stopping VM $id failed"
		fi
		qm destroy "$id" --purge 1 || warn "destroying VM $id failed"
	fi
}
if [ "$pco" = ours ]; then
	systemctl stop pco 2>/dev/null || true
fi
for id in $(seq 9350 9399); do
	destroy_guest "$id"
done

# pco first, while the guests are still there: with them gone, a daemon that
# still ran would see its guests vanish. Against the real API the purge runs
# while the credential is still stored; the fake lived in the suite's process,
# and what it held went with it. A managed network is detached, so that its
# zone and vnet go with the install.
if [ "$pco" = ours ] && command -v pco >/dev/null 2>&1 &&
	{ [ -e "$installfile" ] || [ -e /var/lib/pco/manifest.json ]; }; then
	case "$mode" in
	real) cloudflare=--purge-cloudflare ;;
	fake) cloudflare=--keep-cloudflare ;;
	*) cloudflare= ;;
	esac
	managed=
	[ -e "$state/managed" ] && managed=--detach-managed
	if [ -z "$cloudflare" ]; then
		warn "$owned names no mode of the run, so whether to purge Cloudflare is unknown: pco is left as it is"
		pco=kept
	elif ! pco uninstall --yes "$cloudflare" $managed; then
		warn "pco uninstall $cloudflare $managed failed; pco and its store are kept, and running cleanup.sh again tries again"
		pco=kept
	else
		rm -f "$state/managed"
	fi
fi

if [ -e "$state/foreign" ]; then
	read -r zone id name <"$state/foreign"
	warn "the record $name ($id) that the suite made in the zone $zone may be left at Cloudflare: delete it there" \
		"by hand, then remove $state/foreign"
fi

for id in $(seq 9100 9199); do
	conf=/etc/pve/lxc/$id.conf
	[ -e "$conf" ] || continue
	if ! grep -q '^hostname: e2e-' "$conf"; then
		echo "cleanup: container $id is not one of the suite's; left alone"
		continue
	fi
	pct unlock "$id" >/dev/null 2>&1 || true
	if pct status "$id" | grep -q running; then
		pct stop "$id" || warn "stopping container $id failed"
	fi
	for snap in $(pct listsnapshot "$id" 2>/dev/null | awk '{ print $2 }' | grep -v '^current$'); do
		pct delsnapshot "$id" "$snap" --force 1 || warn "deleting snapshot $snap of $id failed"
	done
	pct destroy "$id" --purge 1 || warn "destroying container $id failed"
done

if [ -e "$dropin" ]; then
	rm -f "$dropin"
	rmdir /etc/systemd/system/pco.service.d 2>/dev/null || true
	systemctl daemon-reload
fi

if [ -e "$state/address-added" ]; then
	if ! ip -4 -o addr show dev "$bridge" | grep -q "inet $address "; then
		rm -f "$state/address-added"
	elif ip addr del "$address" dev "$bridge"; then
		rm -f "$state/address-added"
	else
		warn "removing $address from $bridge failed"
	fi
fi
if [ -e "$state/bridge-was-down" ]; then
	if ip link set dev "$bridge" down; then
		rm -f "$state/bridge-was-down"
	else
		warn "taking $bridge down again failed"
	fi
fi

# The kernel parameters the networking scenarios set go back to their values
# before, and pve-firewall stops again when the suite started it.
if [ -e "$state/sysctl" ]; then
	sysctlfailed=0
	while IFS='=' read -r name value; do
		[ -n "$name" ] || continue
		sysctl -qw "$name=$value" || { warn "putting $name back to $value failed"; sysctlfailed=1; }
	done <"$state/sysctl"
	[ "$sysctlfailed" -eq 0 ] && rm -f "$state/sysctl"
fi
if [ -e "$state/pve-firewall-started" ]; then
	if systemctl stop pve-firewall; then
		rm -f "$state/pve-firewall-started"
	else
		warn "stopping pve-firewall failed"
	fi
fi

if [ "$pco" = ours ]; then
	# Connectors that uninstall could not remove, as when pco was gone already.
	for unit in $(systemctl list-units --all --plain --no-legend 'pco-cloudflared@*' | awk '{ print $1 }'); do
		systemctl disable --now "$unit" || warn "stopping $unit failed"
	done
	if nft list table inet pco_egress >/dev/null 2>&1; then
		nft delete table inet pco_egress || warn "deleting the egress table failed"
	fi

	if dpkg -s pco >/dev/null 2>&1; then
		apt-get purge -y pco || warn "purging the pco package failed"
	fi
	if id pco-connector >/dev/null 2>&1; then
		userdel pco-connector || warn "removing the user pco-connector failed"
	fi
	if getent group pco-connector >/dev/null 2>&1; then
		groupdel pco-connector || warn "removing the group pco-connector failed"
	fi

	# What only pco uninstall removes, and should have.
	if pveum user list --output-format json 2>/dev/null | grep -q '"pco@pve"'; then
		warn "the Proxmox user pco@pve is still there"
	fi
	if pveum role list --output-format json 2>/dev/null | grep -q '"roleid":"PCO"'; then
		warn "the Proxmox role PCO is still there"
	fi
	if [ -d /etc/pve/pco ] || [ -d /etc/pve/priv/pco ]; then
		warn "the store of pco in /etc/pve is still there"
	fi

	# The gateway namespace and the host's tables and routes for it, which
	# only pco uninstall removes, and should have.
	if ip netns list | grep -q '^pco\b'; then
		warn "the gateway namespace pco is still there (pco netns down --uninstall --yes removes it)"
	fi
	for table in "inet pco_host" "bridge pco_guard"; do
		# shellcheck disable=SC2086
		if nft list table $table >/dev/null 2>&1; then
			warn "the table $table is still there"
		fi
	done
	if pvesh get /cluster/sdn/vnets/pconet --output-format json >/dev/null 2>&1; then
		warn "the SDN vnet pconet of the managed network is still there"
	fi
fi

# The runtime bridges of the networking scenarios go last, with nothing
# enslaved to them any more; a VLAN bridge Proxmox made for a tagged guest
# on one of them, <bridge>v<tag>, stays after the guest and goes first.
if [ -e "$state/bridges" ]; then
	bridgesfailed=0
	while read -r name; do
		[ -n "$name" ] || continue
		for vlandir in /sys/class/net/"$name"v[0-9]*; do
			[ -e "$vlandir" ] || continue
			ip link del "$(basename "$vlandir")" || { warn "removing the VLAN bridge $(basename "$vlandir") failed"; bridgesfailed=1; }
		done
		ip link show dev "$name" >/dev/null 2>&1 || continue
		ip link del "$name" || { warn "removing the bridge $name failed"; bridgesfailed=1; }
	done <"$state/bridges"
	[ "$bridgesfailed" -eq 0 ] && rm -f "$state/bridges"
fi

if [ "$failed" -eq 0 ]; then
	rm -rf "$state"
else
	echo "cleanup: $state is kept for the next run of cleanup.sh" >&2
fi
exit "$failed"
