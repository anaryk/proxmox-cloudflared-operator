#!/usr/bin/env bash
#
# Tests for the drop-ins the appliance template puts over the units of the
# package: their lines, one by one, and that the values they name are the
# ones pco uses. Where systemd-analyze is installed, it also checks that
# systemd takes them, beside the units the package installs, without a word;
# with PCO_REQUIRE_SYSTEMD_TESTS=1 a missing systemd-analyze fails the run
# instead of skipping that part.
#
# Run it with: bash packaging/appliance/overlay_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$HERE/../..
UNITS=$HERE/../systemd
DROPINS=$HERE/overlay/etc/systemd/system
CONNECTOR=$DROPINS/pco-cloudflared@.service.d/appliance.conf
DAEMON=$DROPINS/pco.service.d/appliance.conf
WEB=$DROPINS/pco-web.service.d/appliance.conf

CHECKS=0
FAILS=0
SKIPPED=0

# assert <what holds> <command...>
assert() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n' "$what"
	fi
}

same() {
	[[ $1 == "$2" ]] && return
	printf '    got:\n%s\n    want:\n%s\n' "$1" "$2"
	return 1
}

# lines <file>: the lines of a unit file, without comments and empty lines.
lines() {
	grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$1"
}

# defined <file> <pattern>: the Go source defines the constant as the pattern says.
defined() {
	grep -Eq "$2" "$REPO/$1"
}

assert "the connectors start behind pco-net.service and the identity flag, and nothing else is said" \
	same "$(lines "$CONNECTOR")" "[Unit]
Requires=pco-net.service
After=pco-net.service
ConditionPathExists=/run/pco-appliance/identity-ok"

assert "pco starts behind pco-net.service, stays failed on 78 and goes before a connector under memory pressure" \
	same "$(lines "$DAEMON")" "[Unit]
Requires=pco-net.service
After=pco-net.service
[Service]
RestartPreventExitStatus=78
OOMScoreAdjust=500"

assert "pco.service.d/appliance.conf keeps pco from starting again without its volume" \
	grep -qx 'RestartPreventExitStatus=78' "$DAEMON"
assert "78 is the status the daemon exits with without its volume" \
	defined internal/appliance/volume.go '^const ExitNoVolume = 78$'
assert "the flag of the condition is the one the daemon writes" \
	defined internal/appliance/identity.go '^[[:space:]]*IdentityFlag = "/run/pco-appliance/identity-ok"$'

assert "pco-web loads its own pair and the node's API, never pveproxy's certificate, after the daemon" \
	same "$(lines "$WEB")" "[Unit]
After=pco.service
[Service]
LoadCredential=
LoadCredential=tls.crt:/etc/pco/web/tls.crt
LoadCredential=tls.key:/etc/pco/web/tls.key
LoadCredential=pve-api.json:/etc/pco/web/pve-api.json"
assert "pve-api.json is the file the daemon writes and pco web reads" \
	defined internal/webcert/appliance.go '^[[:space:]]*APIName = "pve-api.json"$'
assert "the directory is the one the daemon writes into" \
	defined internal/webcert/files.go '^[[:space:]]*Dir[[:space:]]+= "/etc/pco/web"$'

assert "pco-net.service runs pco net load once, before the network, after the sysctls" \
	same "$(lines "$UNITS/pco-net.service")" "[Unit]
Description=pco service-prefix route and filter of the appliance
Documentation=https://github.com/anaryk/proxmox-cloudflared-operator
DefaultDependencies=no
After=local-fs.target systemd-sysctl.service
Before=network-pre.target pco-egress.service pco.service
Wants=network-pre.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/pco net load
[Install]
WantedBy=multi-user.target"

# verify_cases: systemd-analyze reads the units of the package with the
# drop-ins over them, in a root of their own that holds the units of the
# system as well, and says nothing. It names a key it does not know or a value
# it cannot read and still exits with 0, so whatever it says is a failure.
verify_cases() {
	local root out rc unit
	if ! command -v systemd-analyze >/dev/null 2>&1; then
		if [[ ${PCO_REQUIRE_SYSTEMD_TESTS:-} == 1 ]]; then
			assert "PCO_REQUIRE_SYSTEMD_TESTS=1 but systemd-analyze is not installed" false
		else
			SKIPPED=$((SKIPPED + 1))
			printf 'skipped [systemd-analyze verify]: systemd-analyze is not installed\n'
		fi
		return
	fi
	root=$(mktemp -d "${TMPDIR:-/tmp}/pco-overlay-test.XXXXXXXX") || return
	# shellcheck disable=SC2064
	trap "rm -rf '$root'" EXIT
	mkdir -p "$root/usr/lib/systemd/system" "$root/etc/systemd/system" "$root/usr/bin"
	if [[ -d /usr/lib/systemd/system ]]; then
		cp -R /usr/lib/systemd/system/. "$root/usr/lib/systemd/system/"
	fi
	cp "$UNITS"/*.service "$root/usr/lib/systemd/system/"
	cp -R "$DROPINS/pco.service.d" "$DROPINS/pco-cloudflared@.service.d" "$DROPINS/pco-web.service.d" "$root/etc/systemd/system/"
	for unit in pco cloudflared; do
		printf '#!/bin/sh\n' >"$root/usr/bin/$unit"
		chmod 755 "$root/usr/bin/$unit"
	done

	rc=0
	out=$(systemd-analyze verify --root="$root" pco.service pco-net.service pco-cloudflared@x.service pco-web.service 2>&1) || rc=$?
	assert "systemd-analyze verify takes the units with the drop-ins, exit status $rc: $out" same "$rc:$out" "0:"
	for unit in pco.service pco-cloudflared@x.service pco-web.service; do
		out=$(SYSTEMD_LOG_LEVEL=debug systemd-analyze verify --root="$root" "$unit" 2>&1)
		assert "systemd reads the drop-in of $unit" grep -q "DropIn Path: $root/etc/systemd/system/${unit/@x/@}.d/appliance.conf" <<<"$out"
	done
}
verify_cases

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
