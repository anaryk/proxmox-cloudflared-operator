#!/usr/bin/env bash
#
# Tests for is-latest.sh, in a repository of its own made for the purpose.
#
# Run it with: bash packaging/is-latest_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/is-latest.sh

CASE=
CHECKS=0
FAILS=0
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-is-latest-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

repo() {
	git -C "$ROOT" "$@"
}

repo init -q
repo -c user.name=test -c user.email=test@example.invalid commit -q --allow-empty -m initial

tag() {
	local t
	for t in "$@"; do
		repo tag "$t"
	done
}

# latest <tag> <yes|no>
latest() {
	local rc=0
	CHECKS=$((CHECKS + 1))
	(cd "$ROOT" && bash "$SUT" "$1") >/dev/null 2>&1 || rc=$?
	if [[ $2 == yes && $rc != 0 ]] || [[ $2 == no && $rc != 1 ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] %s: expected %s, rc %s\n' "$CASE" "$1" "$([[ $2 == yes ]] && echo 'the latest' || echo 'not the latest')" "$rc"
	fi
}

CASE='one release'
tag v0.1.0-rc.1 v0.1.0
latest v0.1.0 yes
latest v0.1.0-rc.1 no

CASE='a pre-release of the next version'
tag v0.2.0-rc.1
latest v0.1.0 yes
latest v0.2.0-rc.1 no

CASE='versions are compared as numbers, not as text'
tag v0.9.0 v0.10.0
latest v0.10.0 yes
latest v0.9.0 no
latest v0.1.0 no

CASE='a fix for an older line, tagged after a newer version'
tag v0.9.1
latest v0.9.1 no
latest v0.10.0 yes

CASE='a major version'
tag v1.0.0
latest v1.0.0 yes
latest v0.10.0 no

CASE='a tag that is not there, and one that is not a release'
latest v2.0.0 no
tag vnext v1.0
latest vnext no
latest v1.0 no
latest v1.0.0 yes

CASE='usage'
CHECKS=$((CHECKS + 1))
rc=0
(cd "$ROOT" && bash "$SUT") >/dev/null 2>&1 || rc=$?
if [[ $rc != 2 ]]; then
	FAILS=$((FAILS + 1))
	printf 'FAIL [%s] no argument should be exit status 2, rc %s\n' "$CASE" "$rc"
fi

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
