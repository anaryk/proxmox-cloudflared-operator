#!/usr/bin/env bash
#
# Tests for what postinstall.sh does with pco-netns.service in each profile.
# Each case runs a copy of the script whose absolute paths point into a
# directory of its own, with systemctl, systemd-sysusers and pco replaced by
# stubs that note their calls and keep the state of the units in files.
#
# Run it with: bash packaging/scripts/postinstall_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)

CASE=
CHECKS=0
FAILS=0
RC=0
OUT=
CALLS=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-postinstall-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

# assert <what holds> <command...>
assert() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] %s\n    rc: %s\n    output: %s\n    calls:\n%s\n' "$CASE" "$what" "$RC" "$OUT" "$CALLS"
	fi
}

rc_is() { [[ $RC == "$1" ]]; }
called() { grep -Fxq -- "$1" <<<"$CALLS"; }
not_called() { ! called "$1"; }
contains_out() { [[ $OUT == *"$1"* ]]; }
state_is() { [[ $(cat "$ROOT/node/state/$1" 2>/dev/null) == "$2" ]]; }

mkdir -p "$ROOT/bin"
cat >"$ROOT/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
# The state of a unit is the file state/<unit>: enabled, disabled or masked;
# failed/<unit> says it failed.
printf 'systemctl %s\n' "$*" >>"$NODE/calls"
quiet=0
args=()
for a in "$@"; do
	case $a in
	--quiet) quiet=1 ;;
	*) args+=("$a") ;;
	esac
done
unit=${args[1]:-}
case ${args[0]} in
is-enabled)
	state=$(cat "$NODE/state/$unit" 2>/dev/null) || state=disabled
	[[ $quiet == 1 ]] || printf '%s\n' "$state"
	[[ $state == enabled ]]
	;;
is-failed) [[ -e $NODE/failed/$unit ]] ;;
mask) printf 'masked\n' >"$NODE/state/$unit" ;;
reset-failed) rm -f "$NODE/failed/$unit" ;;
*) ;;
esac
EOF
for tool in systemd-sysusers pco; do
	cat >"$ROOT/bin/$tool" <<'EOF'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >>"$NODE/calls"
EOF
done
chmod +x "$ROOT/bin/"*

# The copy reads its profile, the sign of a running systemd and pco from the
# node of the case.
sed -e "s#/run/systemd/system#\$NODE/run/systemd/system#g" \
	-e "s#/etc/pco/profile#\$NODE/etc/pco/profile#g" \
	-e "s#/usr/bin/pco#$ROOT/bin/pco#g" \
	"$HERE/postinstall.sh" >"$ROOT/postinstall.sh"
# A path of the node left in the copy would be read on the machine the test
# runs on.
if grep -Eq '(^|[^E])/(run/systemd/system|etc/pco/profile)' "$ROOT/postinstall.sh" ||
	grep -Fq /usr/bin/pco "$ROOT/postinstall.sh"; then
	printf 'postinstall_test.sh: the copy of postinstall.sh still names a path of the node\n' >&2
	exit 1
fi

# node <profile|-> <systemd: yes|no> [unit=state...] [failed=unit...]: a new
# node for the next run, with no profile for -.
node() {
	rm -rf "$ROOT/node"
	mkdir -p "$ROOT/node/state" "$ROOT/node/failed" "$ROOT/node/etc/pco"
	[[ $1 == - ]] || printf '%s\n' "$1" >"$ROOT/node/etc/pco/profile"
	[[ $2 == no ]] || mkdir -p "$ROOT/node/run/systemd/system"
	shift 2
	local a
	for a in "$@"; do
		case $a in
		failed=*) : >"$ROOT/node/failed/${a#failed=}" ;;
		*=*) printf '%s\n' "${a#*=}" >"$ROOT/node/state/${a%%=*}" ;;
		esac
	done
}

# run <arguments...>: postinstall as dpkg runs it, leaves RC, OUT and CALLS.
run() {
	: >"$ROOT/node/calls"
	RC=0
	OUT=$(NODE=$ROOT/node PATH="$ROOT/bin:$PATH" sh "$ROOT/postinstall.sh" "$@" 2>&1) || RC=$?
	CALLS=$(<"$ROOT/node/calls")
}

CASE='an appliance upgraded, its pco-netns.service failed'
node appliance yes pco.service=enabled pco-netns.service=enabled failed=pco-netns.service
run configure 0.1.0
assert "passes" rc_is 0
assert "masks pco-netns.service" called "systemctl mask pco-netns.service"
assert "which is masked" state_is pco-netns.service masked
assert "clears its failure" called "systemctl reset-failed pco-netns.service"
assert "with no stop, which the package never sends it" not_called "systemctl stop pco-netns.service"
assert "runs no pco netns up" not_called "pco netns up"
assert "restarts the daemon" called "systemctl try-restart pco.service"

CASE='the same appliance upgraded again'
run configure 0.1.1
assert "passes" rc_is 0
assert "masks nothing again" not_called "systemctl mask pco-netns.service"
assert "stops nothing" not_called "systemctl stop pco-netns.service"
assert "clears nothing" not_called "systemctl reset-failed pco-netns.service"
assert "runs no pco netns up" not_called "pco netns up"

CASE='an appliance whose pco-netns.service did not fail'
node appliance yes pco.service=enabled pco-netns.service=enabled
run configure 0.1.0
assert "masks it" called "systemctl mask pco-netns.service"
assert "clears nothing" not_called "systemctl reset-failed pco-netns.service"

CASE='an appliance installed in a chroot, as the template is built'
node appliance no
run configure ''
assert "passes" rc_is 0
assert "masks pco-netns.service" called "systemctl mask pco-netns.service"
assert "clears nothing without a running systemd" not_called "systemctl reset-failed pco-netns.service"

CASE='a host upgraded, with networks'
node host yes pco.service=enabled pco-netns.service=enabled
run configure 0.1.0
assert "passes" rc_is 0
assert "masks nothing" not_called "systemctl mask pco-netns.service"
assert "leaves pco-netns.service enabled" state_is pco-netns.service enabled
assert "stops nothing" not_called "systemctl stop pco-netns.service"
assert "brings up what this version adds" called "pco netns up"

CASE='a host whose pco-netns.service failed'
node host yes pco.service=enabled pco-netns.service=enabled failed=pco-netns.service
run configure 0.1.0
assert "masks nothing" not_called "systemctl mask pco-netns.service"
assert "stops nothing" not_called "systemctl stop pco-netns.service"
assert "clears nothing" not_called "systemctl reset-failed pco-netns.service"

CASE='a host without a profile file'
node - yes pco.service=enabled pco-netns.service=enabled
run configure 0.1.0
assert "masks nothing" not_called "systemctl mask pco-netns.service"
assert "brings up what this version adds" called "pco netns up"

CASE='a host installed for the first time'
node - yes
run configure ''
assert "passes" rc_is 0
assert "masks nothing" not_called "systemctl mask pco-netns.service"
assert "says pco is not set up" contains_out "pco setup"

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
[[ $FAILS == 0 ]]
