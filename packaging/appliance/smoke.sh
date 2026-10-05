#!/usr/bin/env bash
#
# Checks an appliance template against what build.sh promises: its files
# before the first start, then, booted in systemd-nspawn, from inside.
#
# Usage: sudo packaging/appliance/smoke.sh [--with-network] --version VERSION
#            [--pco-version TEXT] <pco-appliance_VERSION_ARCH.tar.zst>
#
#   --version      the version of pco the template carries, as its file name
#                  has it; the package inside must be that version.
#   --pco-version  what pco version must print as its version, v<VERSION>
#                  unless given. A snapshot's binary says what git describe
#                  says.
#   --with-network boot with the network of the host instead of none, and
#                  check that the first start installed the security updates
#                  from the live archives.
#
# Without --with-network the container has no network at all: the system must
# come up within 180 seconds, running, or degraded by nothing but pco.service
# (pco is not set up) and pco-first-boot.service (no network to update from).
# It needs root, systemd-nspawn, machinectl and systemd-run (systemd-container),
# zstd and dpkg-query, on a host whose systemd runs systemd-machined.

set -euo pipefail

usage() {
	printf 'usage: smoke.sh [--with-network] --version VERSION [--pco-version TEXT] <template.tar.zst>\n' >&2
	exit 2
}

die() {
	printf 'smoke.sh: %s\n' "$*" >&2
	exit 1
}

network=0
version=
pco_version=
while [[ $# -gt 0 ]]; do
	case $1 in
	--with-network) network=1 ;;
	--version | --pco-version)
		[[ $# -ge 2 && -n $2 ]] || usage
		if [[ $1 == --version ]]; then
			version=$2
		else
			pco_version=$2
		fi
		shift
		;;
	-*) usage ;;
	*) break ;;
	esac
	shift
done
[[ $# == 1 && -n $version ]] || usage
template=$1
[[ -f $template && -r $template ]] || die "not a readable file: $template"
pco_version=${pco_version:-v$version}
[[ $(id -u) == 0 ]] || die "systemd-nspawn needs root"
for tool in systemd-nspawn machinectl systemd-run zstd tar dpkg-query; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and was not found"
done

# The start is done when pco.service is: a daemon that waits for something
# the container does not have holds it for up to its start timeout of 150 s.
# The first start with a network also downloads and installs what Debian
# published since the snapshot.
if [[ $network == 1 ]]; then
	limit=600
else
	limit=180
fi

machine=pco-smoke-$$
root=$(mktemp -d /var/tmp/pco-smoke.XXXXXXXX)
log=$root.log
nspawn=

cleanup() {
	if [[ -n $nspawn ]] && kill -0 "$nspawn" 2>/dev/null; then
		machinectl poweroff "$machine" >/dev/null 2>&1 || true
		for _ in $(seq 30); do
			kill -0 "$nspawn" 2>/dev/null || break
			sleep 1
		done
		machinectl terminate "$machine" >/dev/null 2>&1 || true
		kill "$nspawn" 2>/dev/null || true
		wait "$nspawn" 2>/dev/null || true
	fi
	rm -rf "$root" "$log"
}
trap cleanup EXIT

CHECKS=0
FAILS=0

# check <what holds> <command...>
check() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n' "$what"
	fi
}

is() { [[ $1 == "$2" ]]; }
starts() { [[ $1 == "$2"* ]]; }
absent() { [[ ! -e $1 && ! -L $1 ]]; }
empty_file() { [[ -f $1 && ! -L $1 && ! -s $1 ]]; }

zstd --decompress --stdout --long=27 --quiet "$template" | tar --extract --numeric-owner --directory "$root"

# What the build must have left out, read from the tree before the first
# start writes into it: the machine ID is made at that start, the resolver
# comes from Proxmox VE, nothing serves a login, and pco fetches with Go.
check "/etc/machine-id is empty" empty_file "$root/etc/machine-id"
check "/etc/resolv.conf is absent" absent "$root/etc/resolv.conf"
check "/root/.ssh is absent" absent "$root/root/.ssh"
# installed: the packages in the dpkg database of the tree, in any state but
# not-installed, one per line.
# shellcheck disable=SC2016
installed=$(dpkg-query "--admindir=$root/var/lib/dpkg" --show --showformat='${db:Status-Status} ${Package}\n' |
	awk '$1 != "not-installed" { print $2 }') || true
listed() { grep -Fxq -- "$1" <<<"$installed"; }
unlisted() { ! listed "$1"; }
check "the dpkg database of the tree lists pco" listed pco
for package in openssh-server sudo cron curl; do
	check "the package $package is not installed" unlisted "$package"
done

args=(--quiet --boot --directory "$root" --machine "$machine" --console=passive)
if [[ $network == 0 ]]; then
	args+=(--private-network)
fi
systemd-nspawn "${args[@]}" >"$log" 2>&1 &
nspawn=$!
deadline=$((SECONDS + limit))

# inside <command...>: runs it in the container and passes its output and exit
# status on, its arguments as they are: systemd-run would expand ${...}.
inside() {
	systemd-run --machine "$machine" --quiet --wait --pipe --collect --expand-environment=no -- "$@" </dev/null
}

# The container's manager answers once its bus is up.
until inside true 2>/dev/null; do
	if ! kill -0 "$nspawn" 2>/dev/null; then
		cat "$log" >&2
		die "systemd-nspawn ended before the container came up"
	fi
	((SECONDS < deadline)) || die "the container did not come up within $limit seconds"
	sleep 1
done
# timeout 0 would wait for ever.
left=$((deadline - SECONDS))
((left > 0)) || left=1
state=$(timeout "$left" systemd-run --machine "$machine" --quiet --wait --pipe --collect -- \
	systemctl is-system-running --wait </dev/null) || true
state=${state:-not started within $limit seconds}

user_exists() { inside getent passwd "$1" >/dev/null; }

failed=$(inside systemctl list-units --state=failed --plain --no-legend --no-pager | awk '{ print $1 }' | LC_ALL=C sort | paste -s -d ' ' -)

if [[ $network == 0 ]]; then
	# allowed_failed: no unit failed but those of a container without network
	# that pco is not set up in.
	allowed_failed() {
		local unit
		for unit in $failed; do
			case $unit in
			pco.service | pco-first-boot.service) ;;
			*) return 1 ;;
			esac
		done
	}
	case $state in
	running) ;;
	degraded) check "only pco.service and pco-first-boot.service failed, not: $failed" allowed_failed ;;
	*) check "the system is running or degraded, not '$state'" false ;;
	esac

	tilde='~'
	check "pco version says $pco_version" starts "$(inside pco version)" "pco $pco_version ("
	# shellcheck disable=SC2016
	check "the package pco is $version" is "$(inside dpkg-query --show --showformat='${Version}' pco)" "${version/-/$tilde}"
	check "pco egress show finds the table pco-egress.service loaded" starts "$(inside pco egress show || true)" \
		"The egress filter is on."
	check "/etc/pco/profile says appliance" is "$(inside cat /etc/pco/profile)" appliance
	check "pco and cloudflared are held" is "$(inside apt-mark showhold | LC_ALL=C sort | paste -s -d ' ' -)" "cloudflared pco"
	check "the password of root is locked" is "$(inside passwd --status root | awk '{ print $2 }')" L
	check "the user pco-connector exists" user_exists pco-connector
	check "nftables.service is masked" is "$(inside systemctl is-enabled nftables.service || true)" masked
	# shellcheck disable=SC2016
	check "apt reads deb.debian.org and security.debian.org and nothing else" \
		is "$(inside apt-get indextargets --no-release-info --format '$(SITE)' | LC_ALL=C sort -u | paste -s -d ' ' -)" \
		"http://deb.debian.org/debian http://security.debian.org/debian-security"
	check "apt has no Check-Valid-Until option" is "$(inside apt-config dump | grep -ci check-valid-until || true)" 0
	check "/etc/apt/apt.conf.d/99mmdebstrap is gone" inside test ! -e /etc/apt/apt.conf.d/99mmdebstrap
	# security_only: unattended-upgrade allows the origins labelled
	# Debian-Security and no other. It prints them joined by ", ", and each
	# holds commas of its own.
	origins=$(inside unattended-upgrade --dry-run -v 2>&1 |
		sed -n '/Allowed origins are: /{s/^.*Allowed origins are: //;p;q;}' | awk -F', ' '{ for (i = 1; i <= NF; i++) print $i }') || true
	security_only() {
		local origin count=0
		while IFS= read -r origin; do
			[[ -n $origin ]] || continue
			[[ ,$origin, == *,label=Debian-Security,* ]] || return 1
			count=$((count + 1))
		done <<<"$origins"
		((count > 0))
	}
	check "unattended-upgrade allows Debian-Security only, not: ${origins//$'\n'/; }" security_only
else
	# lists_from_live: the package lists were downloaded from the two live
	# archives, from both and from nothing else.
	lists_from_live() {
		local lists name debian=0 security=0
		lists=$(inside find /var/lib/apt/lists -maxdepth 1 -type f ! -name lock -printf '%f\n')
		for name in $lists; do
			case $name in
			deb.debian.org_debian_*) debian=1 ;;
			security.debian.org_debian-security_*) security=1 ;;
			*) return 1 ;;
			esac
		done
		[[ $debian == 1 && $security == 1 ]]
	}
	up() { [[ $state == running || $state == degraded ]]; }
	first=$(inside systemctl show --property=ConditionResult,Result,ExecMainStatus pco-first-boot.service | LC_ALL=C sort | paste -s -d ' ' -)
	check "the system came up, not '$state'" up
	check "pco-first-boot.service ran and succeeded, not: $first" is "$first" "ConditionResult=yes ExecMainStatus=0 Result=success"
	check "/var/lib/misc/pco-first-upgrade exists" inside test -f /var/lib/misc/pco-first-upgrade
	check "the package lists come from deb.debian.org and security.debian.org only" lists_from_live
fi

if [[ $FAILS != 0 ]]; then
	printf '\nThe system is %s; failed units: %s\n' "$state" "${failed:-none}"
	inside journalctl --boot --no-pager --lines=80 || true
fi
printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
[[ $FAILS == 0 ]]
