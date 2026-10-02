#!/bin/sh
set -e

if [ "$1" != remove ] && [ "$1" != purge ]; then
	exit 0
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi

# A plain remove keeps /var/lib/pco, where the tunnel tokens and the install
# record are, so that installing the package again picks up where it was left.
# /etc/pve is never touched.
if [ "$1" = purge ]; then
	rm -rf /var/lib/pco
fi
