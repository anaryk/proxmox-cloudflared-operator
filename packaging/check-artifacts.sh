#!/usr/bin/env bash
#
# Checks what goreleaser left in dist/ against what scripts/install.sh downloads:
# pco_<version>_<arch>.deb for amd64 and arm64, and checksums.txt with one line
# "<sha256>  <file name>" for each package, as sha256sum writes it. The version
# is the tag without the v, a pre-release suffix kept as it is. Each package must
# also say, in its control file, that it is pco, of that version and of the
# architecture in its name; dpkg writes a pre-release as 0.1.0~rc.1 there.
#
# Usage: packaging/check-artifacts.sh [--signed] [--version VERSION] [--require-dpkg-deb] [--require-ui] [dist-dir]
#
#   --version  the version the release is meant to have. Without it the check
#              trusts dist/metadata.json for the version, which is right for
#              a snapshot and proves nothing in a release: goreleaser picks the
#              tag it builds. With it, metadata.json and every file name must agree.
#   --signed   a release also holds checksums.txt.sig; a snapshot has none.
#   --require-dpkg-deb
#              fail when dpkg-deb is missing, instead of skipping the control
#              fields with a note. CI and the release set it.
#   --require-ui
#              read ./usr/bin/pco out of each package and fail unless go
#              version -m says it was built with -tags=nomsgpack,webui: without
#              webui it serves a page that says it has no web interface. Needs
#              dpkg-deb and go. CI, the release and make snapshot set it.
#
# Nothing else may be in dist/ besides the files goreleaser keeps for itself.

set -euo pipefail

usage() {
	printf 'usage: check-artifacts.sh [--signed] [--version VERSION] [--require-dpkg-deb] [--require-ui] [dist-dir]\n' >&2
	exit 2
}

signed=0
want=
require_dpkg=0
require_ui=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--signed) signed=1 ;;
	--require-dpkg-deb) require_dpkg=1 ;;
	--require-ui) require_ui=1 ;;
	--version)
		[[ $# -ge 2 && -n $2 ]] || usage
		want=$2
		shift
		;;
	-*) usage ;;
	*) break ;;
	esac
	shift
done
[[ $# -le 1 ]] || usage
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

# field <json file> <name>: the string value of a top level key of metadata.json.
field() {
	sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1"
}

meta=$dist/metadata.json
if [[ ! -f $meta ]]; then
	printf 'check-artifacts.sh: %s not found, run goreleaser first\n' "$meta" >&2
	exit 1
fi
version=$(field "$meta" version)
tag=$(field "$meta" tag)
if [[ -z $version ]]; then
	printf 'check-artifacts.sh: %s names no version\n' "$meta" >&2
	exit 1
fi
if [[ -n $want ]]; then
	if [[ $version != "$want" ]]; then
		fail "goreleaser built version $version, the release is for $want"
	fi
	if [[ $tag != "v$want" ]]; then
		fail "goreleaser built from the tag ${tag:-<none>}, the release is for v$want"
	fi
	version=$want
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

if command -v dpkg-deb >/dev/null 2>&1; then
	tilde='~'
	deb_version=${version/-/$tilde}
	for arch in amd64 arm64; do
		deb=$dist/pco_${version}_$arch.deb
		[[ -f $deb ]] || continue
		got_package=$(dpkg-deb -f "$deb" Package 2>/dev/null) || got_package=
		got_version=$(dpkg-deb -f "$deb" Version 2>/dev/null) || got_version=
		got_arch=$(dpkg-deb -f "$deb" Architecture 2>/dev/null) || got_arch=
		if [[ -z $got_package && -z $got_version && -z $got_arch ]]; then
			fail "dpkg-deb cannot read the control file of ${deb##*/}"
			continue
		fi
		if [[ $got_package != pco ]]; then
			fail "${deb##*/} is the package ${got_package:-<none>}, expected pco"
		fi
		if [[ $got_version != "$deb_version" ]]; then
			fail "${deb##*/} has the version ${got_version:-<none>}, expected $deb_version"
		fi
		if [[ $got_arch != "$arch" ]]; then
			fail "${deb##*/} is built for ${got_arch:-<none>}, expected $arch"
		fi
	done
elif [[ $require_dpkg == 1 ]]; then
	fail "dpkg-deb is required to read the control files of the packages and was not found"
else
	printf 'check-artifacts.sh: dpkg-deb not found, the control files of the packages are not checked\n' >&2
fi

# tags <go binary>: the build tags go version -m lists for it, empty for none.
tags() {
	go version -m "$1" | awk -F'\t' '$2 == "build" && $3 ~ /^-tags=/ { sub(/^-tags=/, "", $3); print $3 }'
}

if [[ $require_ui == 1 ]]; then
	if ! command -v dpkg-deb >/dev/null 2>&1; then
		fail "dpkg-deb is required to read the binaries of the packages (--require-ui) and was not found"
	elif ! command -v go >/dev/null 2>&1; then
		fail "go is required to read the build tags of the binaries (--require-ui) and was not found"
	else
		work=$(mktemp -d "${TMPDIR:-/tmp}/check-artifacts.XXXXXXXX")
		trap 'rm -rf "$work"' EXIT
		for arch in amd64 arm64; do
			deb=$dist/pco_${version}_$arch.deb
			[[ -f $deb ]] || continue
			if ! dpkg-deb --fsys-tarfile "$deb" 2>/dev/null | tar -xO ./usr/bin/pco >"$work/pco" 2>/dev/null; then
				fail "${deb##*/} has no ./usr/bin/pco"
				continue
			fi
			if ! got_tags=$(tags "$work/pco" 2>/dev/null); then
				fail "go version -m cannot read ./usr/bin/pco of ${deb##*/}"
				continue
			fi
			if [[ $got_tags != nomsgpack,webui ]]; then
				if [[ -n $got_tags ]]; then
					built="built with -tags=$got_tags"
				else
					built="built without tags"
				fi
				fail "${deb##*/} carries a pco $built; a release needs -tags=nomsgpack,webui, or it serves a page that says it has no web interface"
			fi
		done
	fi
fi

if [[ $failures != 0 ]]; then
	exit 1
fi
printf 'check-artifacts.sh: %s holds the release files for %s\n' "$dist" "$version"
