#!/usr/bin/env bash
#
# Tests for what build.sh takes from pin.conf, through its dry run, which
# downloads nothing. Each case runs a copy of build.sh beside a pin.conf of
# its own. It takes jq.
#
# Run it with: bash packaging/appliance/pin_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SHA=9ea7778e443144ca490668737a8ab22dd3e748bb99e805e22ec055abeb3c7fac
KEYRING=pool/main/d/debian-archive-keyring/debian-archive-keyring_2025.1_all.deb

CASE=
CHECKS=0
FAILS=0
RC=0
OUT=
ERR=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-pin-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

mkdir -p "$ROOT/appliance"
cp "$HERE/build.sh" "$HERE/packages.txt" "$ROOT/appliance/"
cp "$HERE/../cloudflared-versions.json" "$ROOT/"
: >"$ROOT/pco.deb"

# assert <what holds> <command...>
assert() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] %s\n    rc: %s\n    stdout: %s\n    stderr: %s\n' "$CASE" "$what" "$RC" "$OUT" "$ERR"
	fi
}

rc_is() { [[ $RC == "$1" ]]; }
contains() { [[ $1 == *"$2"* ]]; }
lacks() { [[ $1 != *"$2"* ]]; }
matches() { [[ $1 =~ $2 ]]; }

# pin <lines...>: the pin.conf of the next run.
pin() {
	printf '%s\n' "$@" >"$ROOT/appliance/pin.conf"
}

# run <arguments...>: a dry run for amd64, leaves RC, OUT and ERR.
run() {
	RC=0
	bash "$ROOT/appliance/build.sh" --dry-run --arch amd64 --version 0.1.0 --deb "$ROOT/pco.deb" "$@" \
		>"$ROOT/out" 2>"$ROOT/err" || RC=$?
	OUT=$(<"$ROOT/out")
	ERR=$(<"$ROOT/err")
}

CASE='the pin.conf of the repository'
cp "$HERE/pin.conf" "$ROOT/appliance/pin.conf"
run
assert "passes" rc_is 0
assert "hands mmdebstrap the keyring it downloads" matches "$OUT" ' --keyring=[^ ]*/pco-appliance-build\.dry/debian-archive-keyring\.pgp '

CASE='the keyring'
pin '# a comment' 'SNAPSHOT=20261004T000000Z' 'KEYRING_VERSION=2025.1' "KEYRING_SHA256=$SHA"
run
assert "passes" rc_is 0
assert "comes from the snapshot" contains "$OUT" \
	"debian-archive-keyring 2025.1 from https://snapshot.debian.org/archive/debian/20261004T000000Z/$KEYRING, sha256 $SHA"
assert "which the build is made from" contains "$OUT" "http://snapshot.debian.org/archive/debian/20261004T000000Z/"

CASE='a build from another snapshot'
run --snapshot 20260901T000000Z
assert "passes" rc_is 0
assert "takes the keyring from the snapshot of pin.conf" contains "$OUT" \
	"https://snapshot.debian.org/archive/debian/20261004T000000Z/$KEYRING, sha256 $SHA"
assert "and the packages from the other" contains "$OUT" "http://snapshot.debian.org/archive/debian/20260901T000000Z/"
assert "and from no other" lacks "$OUT" "http://snapshot.debian.org/archive/debian/20261004T000000Z/"

CASE='no sha256'
pin 'SNAPSHOT=20261004T000000Z' 'KEYRING_VERSION=2025.1'
run
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "pin.conf has no sha256 of debian-archive-keyring"

CASE='a sha256 that is not one'
pin 'SNAPSHOT=20261004T000000Z' 'KEYRING_VERSION=2025.1' "KEYRING_SHA256=${SHA:1}"
run
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "pin.conf has no sha256 of debian-archive-keyring"

CASE='no version'
pin 'SNAPSHOT=20261004T000000Z' "KEYRING_SHA256=$SHA"
run
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "pin.conf names no version of debian-archive-keyring"

CASE='a version that is a path'
pin 'SNAPSHOT=20261004T000000Z' 'KEYRING_VERSION=../2025.1' "KEYRING_SHA256=$SHA"
run
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "pin.conf names no version of debian-archive-keyring"

CASE='no snapshot, also when the build names one'
pin 'KEYRING_VERSION=2025.1' "KEYRING_SHA256=$SHA"
run --snapshot 20261004T000000Z
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "pin.conf names no snapshot"

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
