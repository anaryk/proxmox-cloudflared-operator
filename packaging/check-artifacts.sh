#!/usr/bin/env bash
#
# Checks what goreleaser left in dist/ against what scripts/install.sh downloads:
# pco_<version>_<arch>.deb for amd64 and arm64, and checksums.txt with one line
# "<sha256>  <file name>" for each package, as sha256sum writes it. The version
# is the one goreleaser records in dist/metadata.json: the tag without the v,
# a pre-release suffix kept as it is.
#
# Usage: packaging/check-artifacts.sh [--signed] [dist-dir]
#
# A release also holds checksums.txt.sig and is checked with --signed; a
# snapshot is built without signing and has none. Nothing else may be there
# besides the files goreleaser keeps for itself.

set -euo pipefail

signed=0
if [[ ${1:-} == --signed ]]; then
	signed=1
	shift
fi
if [[ $# -gt 1 ]]; then
	printf 'usage: check-artifacts.sh [--signed] [dist-dir]\n' >&2
	exit 2
fi
dist=${1:-dist}

failures=0
fail() {
	printf 'check-artifacts.sh: %s\n' "$*" >&2
	failures=$((failures + 1))
}

sha256() {
	local out
	if command -v sha256sum >/dev/null 2>&1; then
		out=$(sha256sum "$1")
	else
		out=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${out%% *}"
}

meta=$dist/metadata.json
if [[ ! -f $meta ]]; then
	printf 'check-artifacts.sh: %s not found, run goreleaser first\n' "$meta" >&2
	exit 1
fi
version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$meta")
if [[ -z $version ]]; then
	printf 'check-artifacts.sh: %s names no version\n' "$meta" >&2
	exit 1
fi

debs="pco_${version}_amd64.deb
pco_${version}_arm64.deb"
expected="$debs
checksums.txt"
if [[ $signed == 1 ]]; then
	expected="$expected
checksums.txt.sig"
fi

actual=
for path in "$dist"/*; do
	[[ -f $path ]] || continue
	name=${path##*/}
	case $name in
	artifacts.json | config.yaml | metadata.json | CHANGELOG.md) continue ;;
	esac
	actual="$actual$name
"
done

while IFS= read -r name; do
	if ! printf '%s' "$actual" | grep -Fxq -- "$name"; then
		fail "$dist has no $name"
	fi
done <<<"$expected"
while IFS= read -r name; do
	[[ -n $name ]] || continue
	if ! printf '%s\n' "$expected" | grep -Fxq -- "$name"; then
		fail "$dist holds $name, which the release is not meant to have"
	fi
done <<<"$actual"

if [[ -f $dist/checksums.txt ]]; then
	re='^([0-9a-f]{64})  (.+)$'
	listed=
	while IFS= read -r line || [[ -n $line ]]; do
		if ! [[ $line =~ $re ]]; then
			fail "checksums.txt has a line that is not \"<sha256>  <file name>\": $line"
			continue
		fi
		hash=${BASH_REMATCH[1]}
		name=${BASH_REMATCH[2]}
		if ! printf '%s\n' "$debs" | grep -Fxq -- "$name"; then
			fail "checksums.txt lists $name, which is not a package of this release"
			continue
		fi
		listed="$listed$name
"
		if [[ -f $dist/$name && $(sha256 "$dist/$name") != "$hash" ]]; then
			fail "checksums.txt has the wrong checksum for $name"
		fi
	done <"$dist/checksums.txt"
	while IFS= read -r name; do
		count=$(printf '%s' "$listed" | grep -Fxc -- "$name" || true)
		if [[ $count != 1 ]]; then
			fail "checksums.txt has $count lines for $name, expected one"
		fi
	done <<<"$debs"
fi

if [[ $failures != 0 ]]; then
	exit 1
fi
printf 'check-artifacts.sh: %s holds the release files for %s\n' "$dist" "$version"
