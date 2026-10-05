#!/bin/sh
set -e

# dpkg also runs postinst for aborted upgrades and removals; those need nothing.
if [ "$1" != configure ]; then
	exit 0
fi

# The connectors run as pco-connector, and the egress filter matches that user;
# the web interface runs as pco-web.
systemd-sysusers /usr/lib/sysusers.d/pco.conf

# $2 is the version that was configured before. It is empty on a first install,
# but not after a remove that kept the configuration files, and prerm disabled
# the daemon then: the unit is looked at as well.
not_set_up=
if [ -z "$2" ]; then
	not_set_up=1
fi

# Without a running systemd (a chroot, a container build) there is nothing to
# reload or restart. A failing systemctl must not leave dpkg with a package
# stuck half configured, so its errors are shown and not fatal.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	# Only an upgrade restarts the daemon and the web interface, and only those
	# that were running; pco setup enables the web interface. The
	# pco-cloudflared@ connectors are never touched: restarting them would drop
	# the tunnels they serve.
	if [ -n "$2" ]; then
		systemctl try-restart pco.service || true
		systemctl try-restart pco-web.service || true
	fi
	if ! systemctl is-enabled --quiet pco.service 2>/dev/null; then
		not_set_up=1
	fi
fi

if [ -n "$not_set_up" ]; then
	echo "pco is installed, but pco.service is not enabled."
	echo "Run 'pco setup' as root to set pco up, or to bring it back after a removal."
fi
