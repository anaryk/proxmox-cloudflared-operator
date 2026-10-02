#!/bin/sh
set -e

# The old package runs its prerm on an upgrade too, with "upgrade" as the argument.
if [ "$1" != remove ]; then
	exit 0
fi

# Only the operator is stopped. The pco-cloudflared@ connectors keep running so
# that removing the package does not take the tunnels down; `pco uninstall`
# removes them, and has to run while the binary is still there. A failing stop
# is shown, but must not keep the package from being removed.
if [ -d /run/systemd/system ]; then
	if systemctl cat pco.service >/dev/null 2>&1; then
		systemctl disable --now pco.service || true
	fi
	if [ -n "$(systemctl list-units --plain --no-legend 'pco-cloudflared@*' 2>/dev/null)" ]; then
		echo "The pco-cloudflared connectors keep running after this removal."
		echo "Only 'pco uninstall' removes them, and it needs pco to be installed."
	fi
fi
