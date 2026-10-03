#!/bin/sh
# Removes from a Proxmox VE node what the end-to-end suite and its run leave
# behind, also after a run that was cut off: the containers 9100 to 9199 named
# e2e-*, pco set up against the fake, the drop-in of pco.service, the address
# of the node on vmbr1, the pco package and the user of the connectors. It
# runs as root on the node and can be run again.
set -u

bridge=vmbr1
address=10.77.0.1/24
marker=/run/pco-e2e-$bridge-was-down
dropin=/etc/systemd/system/pco.service.d/e2e.conf
failed=0

warn() {
	echo "cleanup: $*" >&2
	failed=1
}

if [ "$(id -u)" -ne 0 ]; then
	echo "cleanup: run as root" >&2
	exit 2
fi

# pco first, while the guests are still there: with them gone, a daemon that
# still ran would see its guests vanish. Cloudflare is left as it is: the fake
# lived in the suite's process, and the real API is the suite's to purge.
if command -v pco >/dev/null 2>&1 &&
	{ [ -e /etc/pve/pco/meta/install.json ] || [ -e /var/lib/pco/manifest.json ]; }; then
	pco uninstall --yes --keep-cloudflare || warn "pco uninstall failed"
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

if ip -4 -o addr show dev "$bridge" | grep -q " $address "; then
	ip addr del "$address" dev "$bridge" || warn "removing $address from $bridge failed"
fi
# The suite brings the bridge up when it was down, and leaves this behind
# until it took it down again.
if [ -e "$marker" ]; then
	ip link set dev "$bridge" down || warn "taking $bridge down again failed"
	rm -f "$marker"
fi

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

exit "$failed"
