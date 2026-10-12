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

# pco-netns.service brings up the gateway namespace of the host profile. The
# appliance has none, as its legs are network cards of the container, and
# there the unit, which the presets of systemd enable at the first start,
# fails at every boot. The template masks it; this masks it in an appliance
# made from an older one. A host is never touched.
appliance=
if [ "$(cat /etc/pco/profile 2>/dev/null || true)" = appliance ]; then
	appliance=1
	if [ "$(systemctl is-enabled pco-netns.service 2>/dev/null || true)" != masked ]; then
		systemctl mask pco-netns.service || true
	fi
fi

# Without a running systemd (a chroot, a container build) there is nothing to
# reload or restart. A failing systemctl must not leave dpkg with a package
# stuck half configured, so its errors are shown and not fatal.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	# A failure of the unit from before the mask leaves the appliance degraded
	# until it is cleared. The failed one-shot runs nothing that a stop would
	# end.
	if [ -n "$appliance" ] && systemctl is-failed --quiet pco-netns.service 2>/dev/null; then
		systemctl reset-failed pco-netns.service || true
	fi
	# Only an upgrade restarts the daemon and the web interface, and only those
	# that were running; pco setup enables the web interface. The
	# pco-cloudflared@ connectors are never touched: restarting them would drop
	# the tunnels they serve. Neither is pco-netns.service, which has no stop
	# action: the legs, the service addresses and the routes it brings up carry
	# what the connectors serve. Where it is enabled, pco netns up brings up
	# what this version adds before the daemon restarts, and changes nothing
	# that is in place.
	if [ -n "$2" ]; then
		if systemctl is-enabled --quiet pco-netns.service 2>/dev/null; then
			/usr/bin/pco netns up || true
		fi
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
