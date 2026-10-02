#!/bin/sh
set -e

if [ "$1" != remove ] && [ "$1" != purge ]; then
	exit 0
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi

# A plain remove keeps /var/lib/pco, where the tunnel tokens and the install
# record are, for `pco setup` to bring the installation back after a reinstall.
# The daemon is not enabled again by the package. /etc/pve is never touched.
if [ "$1" = purge ]; then
	rm -rf /var/lib/pco
fi
