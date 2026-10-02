#!/bin/sh
set -e

# dpkg also runs postinst for aborted upgrades and removals; those need nothing.
if [ "$1" != configure ]; then
	exit 0
fi

if [ -f /usr/lib/sysusers.d/pco.conf ] && command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers /usr/lib/sysusers.d/pco.conf
fi

# Without a running systemd (a chroot, a container build) there is nothing to
# reload or restart. A failing systemctl must not leave dpkg with a package
# stuck half configured, so its errors are shown and not fatal.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	# $2 is the version that was configured before, so only an upgrade restarts
	# the daemon, and only if it was running. The pco-cloudflared@ connectors
	# are never touched: restarting them would drop the tunnels they serve.
	if [ -n "$2" ]; then
		systemctl try-restart pco.service || true
	fi
fi

if [ -z "$2" ]; then
	echo "pco is installed but not set up yet."
	echo "Run 'pco setup' as root to connect it to Cloudflare and Proxmox VE."
fi
