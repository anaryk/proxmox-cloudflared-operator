#!/bin/sh
# Removes from the nodes of a cluster what a run of the cluster profile of
# the end-to-end suite leaves behind, also after a run that was cut off. It
# runs as root on the node the suite ran on, and reaches the other nodes with
# ssh as root, as the suite did. It acts only on what the suite recorded in
# /var/lib/pco-e2e/cluster: the nodes, the install it set up (uninstalled from
# every node when the cluster's store still names it), its guests 9500 to
# 9549 named e2e-*, the cuts it made with nftables, the relay and the drop-ins
# of pco.service, the runtime bridges of its networking scenarios, the files
# it copied, and the registered tags it changed. It can be run again.
#
# After a run of the appliance on a cluster, run cleanup.sh in its appliance
# mode (PCO_E2E_PROFILE=appliance) on every node with an agent container
# first, then on the node the suite ran on.

set -u

state=/var/lib/pco-e2e
record=$state/cluster
installfile=/etc/pve/pco/meta/install.json

# warn reports what is left. The loops below run in subshells, so what failed
# is counted in a file.
warn() {
	echo "cleanup-cluster: $*" >&2
	echo "$*" >>"$state/failed"
}

if [ "$(id -u)" -ne 0 ]; then
	echo "cleanup-cluster: run as root" >&2
	exit 2
fi
if [ ! -e "$record" ]; then
	echo "cleanup-cluster: $record is missing: no run of the cluster profile left anything to remove here"
	exit 0
fi

self=$(perl -MJSON::PP -e 'local $/; print decode_json(<STDIN>)->{nodename} // ""' </etc/pve/.members)
ours=$(sed -n 's/^install=//p' "$record" | head -n 1)
mode=$(sed -n 's/^mode=//p' "$record" | head -n 1)

# on runs a command line on a node: here on the node itself, with ssh on the
# others, the host key looked up under the node's name. Neither reads the
# input of the loop it runs in.
on() {
	node=$1
	addr=$2
	shift 2
	if [ "$node" = "$self" ]; then
		sh -c "$*" </dev/null
	else
		ssh -n -o BatchMode=yes -o ConnectTimeout=10 -o HostKeyAlias="$node" -o LogLevel=ERROR "root@$addr" -- "$*"
	fi
}

nodes=$(sed -n 's/^node=//p' "$record")

rm -f "$state/failed"

# A node that does not answer keeps what the run left on it.
echo "$nodes" | while read -r node addr; do
	[ -n "$node" ] || continue
	if ! on "$node" "$addr" true; then
		echo "$node"
	fi
done >"$state/unreachable"
while read -r node; do
	warn "node $node does not answer: what the run left there stays; run this again once it is back"
done <"$state/unreachable"

installed=
if [ -e "$installfile" ]; then
	installed=$(perl -MJSON::PP -e 'local $/; my $d = decode_json(<STDIN>); print $d->{data}{id} // ""' <"$installfile")
fi

# The cuts first, so that the nodes reach each other again.
echo "$nodes" | while read -r node addr; do
	[ -n "$node" ] || continue
	grep -qx "$node" "$state/unreachable" && continue
	on "$node" "$addr" "nft delete table inet pco_e2e_cut 2>/dev/null; rm -f /run/pco_e2e_cut.nft /run/pco-e2e-load-stop; rm -rf /run/pco-e2e-cert; true"
	on "$node" "$addr" "systemctl kill --signal=SIGCONT pco.service 2>/dev/null; true"
done

# pco goes from every node while the store names the run's install: the
# nodes that are not this one first, this one last, which purges what the
# install has at Cloudflare against the real API.
if [ -n "$installed" ] && [ "$installed" = "$ours" ]; then
	echo "$nodes" | while read -r node addr; do
		[ -n "$node" ] && [ "$node" != "$self" ] || continue
		grep -qx "$node" "$state/unreachable" && continue
		on "$node" "$addr" "test -x /usr/bin/pco || exit 0; pco uninstall --yes --keep-cloudflare --force" ||
			warn "pco uninstall on $node failed"
	done
	purge=--keep-cloudflare
	[ "$mode" = real ] && purge=--purge-cloudflare
	if ! pco uninstall --yes "$purge" --force --detach-managed; then
		warn "pco uninstall on $self failed"
	fi
elif [ -n "$installed" ]; then
	warn "the store names install $installed, which the run did not record ($record names ${ours:-none}): pco is left alone"
fi
if [ "$mode" = real ] && [ -n "$ours" ]; then
	echo "cleanup-cluster: against the real API, check that the tunnels pco-$ours and pco-$ours-<node> and the records commented pco:$ours are gone"
fi

# The guests of the run, wherever they are now.
pvesh get /cluster/resources --type vm --output-format json 2>/dev/null |
	perl -MJSON::PP -e 'local $/; for (@{decode_json(<STDIN>)}) { print "$_->{vmid} $_->{node} $_->{type} $_->{name}\n" if $_->{vmid} >= 9500 && $_->{vmid} <= 9549 }' |
	while read -r id node type name; do
		case $name in
		e2e-*) ;;
		*)
			echo "cleanup-cluster: guest $id ($name) on $node is not one of the run's; left alone"
			continue
			;;
		esac
		addr=$(echo "$nodes" | awk -v n="$node" '$1 == n { print $2 }')
		tool=pct
		[ "$type" = qemu ] && tool=qm
		on "$node" "$addr" "$tool stop $id >/dev/null 2>&1; $tool destroy $id --purge 1" || warn "destroying guest $id on $node failed"
	done

# The relay, the drop-ins, the runtime bridges and the files the run copied.
echo "$nodes" | while read -r node addr; do
	[ -n "$node" ] || continue
	grep -qx "$node" "$state/unreachable" && continue
	on "$node" "$addr" "systemctl disable --now e2e-relay.service 2>/dev/null; \
		rm -f /etc/systemd/system/e2e-relay.service /usr/local/bin/e2e-relay \
			/etc/systemd/system/pco.service.d/e2e.conf /etc/systemd/system/pco.service.d/e2e-watchdog.conf \
			/root/e2e-upgrade.deb /root/e2e-cf-token /root/e2e-agent.join; \
		rmdir /etc/systemd/system/pco.service.d 2>/dev/null; systemctl daemon-reload; \
		for b in pcotc1 pcotci; do ip link show dev \$b >/dev/null 2>&1 && ip link del \$b; done; true" ||
		warn "removing what the run left on $node failed"
done

if [ -e "$state/registered-tags" ]; then
	read -r tags <"$state/registered-tags" || tags=
	if [ -n "$tags" ]; then
		pvesh set /cluster/options --registered-tags "$tags" || warn "putting the registered tags back failed"
	else
		pvesh set /cluster/options --delete registered-tags || warn "putting the registered tags back failed"
	fi
	rm -f "$state/registered-tags"
fi

if [ -s "$state/unreachable" ] || [ -s "$state/failed" ]; then
	exit 1
fi
rm -f "$state/unreachable" "$state/failed" "$state/cf-token" "$state/owned" "$record"
rmdir "$state" 2>/dev/null || true
echo "cleanup-cluster: done"
