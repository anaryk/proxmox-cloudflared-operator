#!/usr/bin/env bash
#
# Checks what goreleaser left in dist/ against what scripts/install.sh downloads:
# pco_<version>_<arch>.deb for amd64 and arm64, and checksums.txt with one line
# "<sha256>  <file name>" for each package, as sha256sum writes it. The version
# is the tag without the v, a pre-release suffix kept as it is. Each package must
# also say, in its control file, that it is pco, of that version and of the
# architecture in its name; dpkg writes a pre-release as 0.1.0~rc.1 there. And
# it must list the systemd units of pco, which the appliance template enables,
# the man page of pco and the completion scripts of bash, zsh and fish.
#
# Usage: packaging/check-artifacts.sh [--signed] [--version VERSION] [--require-dpkg-deb] [--require-ui]
#            [--require-template] [dist-dir]
#
#   --version  the version the release is meant to have. Without it the check
#              trusts dist/metadata.json for the version, which is right for
#              a snapshot and proves nothing in a release: goreleaser picks the
#              tag it builds. With it, metadata.json and every file name must agree.
#   --signed   a release also holds checksums.txt.sig; a snapshot has none. And
#              ./usr/share/pco/release-key.gpg in each package, which pco
#              upgrade checks releases with, must hold the release keys of
#              scripts/install.sh and no other. That takes dpkg-deb and gpg.
#   --require-dpkg-deb
#              fail when dpkg-deb is missing, instead of skipping the control
#              fields with a note. CI and the release set it.
#   --require-ui
#              read ./usr/bin/pco out of each package and fail unless go
#              version -m says it was built with -tags=nomsgpack,webui: without
#              webui it serves a page that says it has no web interface. Needs
#              dpkg-deb and go. CI, the release and make snapshot set it.
#   --require-template
#              the release also carries the appliance templates and what goes
#              with them, which goreleaser lists in checksums.txt from the
#              directory PCO_APPLIANCE_DIR names, build/appliance unless set:
#              pco-appliance_<version>_<arch>.tar.zst and .spdx.json for both
#              architectures, pco-appliance_<version>.pin.conf and
#              cloudflared-versions.json. Each template must carry the package
#              of this release: its pco-appliance_<version>_<arch>.deb.sha256,
#              which build.sh writes beside it, must hold the sha256 of
#              pco_<version>_<arch>.deb, and its ./usr/bin/pco must be the
#              one in that package, byte for byte. Its ./usr/bin/cloudflared
#              must be the one in cloudflared_<cf>_<arch>.deb in the same
#              directory, whose sha256 is the one cloudflared-versions.json
#              beside this script lists for the newest version it allows;
#              the copy of that file in the directory must be the same file.
#              Needs dpkg-deb, zstd and jq. The release sets it.
#
# Nothing else may be in dist/ besides the files goreleaser keeps for itself,
# and checksums.txt may list nothing else.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

usage() {
	printf 'usage: check-artifacts.sh [--signed] [--version VERSION] [--require-dpkg-deb] [--require-ui] [--require-template] [dist-dir]\n' >&2
	exit 2
}

signed=0
want=
require_dpkg=0
require_ui=0
require_template=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--signed) signed=1 ;;
	--require-dpkg-deb) require_dpkg=1 ;;
	--require-ui) require_ui=1 ;;
	--require-template) require_template=1 ;;
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

# Where the programs read out of the packages and the templates go.
work=
if [[ $require_ui == 1 || $require_template == 1 ]]; then
	work=$(mktemp -d "${TMPDIR:-/tmp}/check-artifacts.XXXXXXXX")
	trap 'rm -rf "$work"' EXIT
fi

# The files of the appliance, which goreleaser lists and uploads from where
# build.sh wrote them rather than from dist/.
appliance=${PCO_APPLIANCE_DIR:-build/appliance}
templates=
if [[ $require_template == 1 ]]; then
	templates="pco-appliance_${version}_amd64.tar.zst
pco-appliance_${version}_amd64.spdx.json
pco-appliance_${version}_arm64.tar.zst
pco-appliance_${version}_arm64.spdx.json
pco-appliance_${version}.pin.conf
cloudflared-versions.json"
	while IFS= read -r name; do
		if [[ ! -f $appliance/$name ]]; then
			fail "$appliance has no $name"
		fi
	done <<<"$templates"
fi
files=$debs${templates:+
$templates}

# where <name>: the path of a file the checksums list.
where() {
	if printf '%s\n' "$debs" | grep -Fxq -- "$1"; then
		printf '%s\n' "$dist/$1"
	else
		printf '%s\n' "$appliance/$1"
	fi
}

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
		if ! printf '%s\n' "$files" | grep -Fxq -- "$name"; then
			fail "checksums.txt lists $name, which is not a file of this release"
			continue
		fi
		listed="$listed$name
"
		file=$(where "$name")
		if [[ -f $file && $(sha256 "$file") != "$hash" ]]; then
			fail "checksums.txt has the wrong checksum for $name"
		fi
	done <"$dist/checksums.txt"
	while IFS= read -r name; do
		count=$(printf '%s' "$listed" | grep -Fxc -- "$name" || true)
		if [[ $count != 1 ]]; then
			fail "checksums.txt has $count lines for $name, expected one"
		fi
	done <<<"$files"
fi

# carries <arch> <name> <template> <package>: fails unless ./usr/bin/<name> of
# the template is the one in the package, byte for byte.
carries() {
	if ! zstd --decompress --stdout --long=27 --quiet "$3" 2>/dev/null | tar -xO "./usr/bin/$2" >"$work/template" 2>/dev/null; then
		fail "cannot read ./usr/bin/$2 out of ${3##*/}"
	elif ! dpkg-deb --fsys-tarfile "$4" 2>/dev/null | tar -xO "./usr/bin/$2" >"$work/package" 2>/dev/null; then
		fail "cannot read ./usr/bin/$2 out of ${4##*/}"
	elif ! cmp -s "$work/template" "$work/package"; then
		fail "the $1 template carries a /usr/bin/$2 other than the one in ${4##*/}"
	fi
}

if [[ $require_template == 1 ]]; then
	missing=
	for tool in dpkg-deb zstd jq; do
		command -v "$tool" >/dev/null 2>&1 || missing="${missing:+$missing, }$tool"
	done
	if [[ -n $missing ]]; then
		fail "--require-template reads the programs out of the templates and the packages, which needs dpkg-deb, zstd and jq; not found: $missing"
	fi
	# The cloudflared the templates carry: the newest version the manifest
	# of the repository allows, as build.sh picks it, and the sha256 listed
	# there. The copy that ships with the release must be that file.
	manifest=$HERE/cloudflared-versions.json
	cf_version=
	if [[ -z $missing ]]; then
		cf_version=$(jq -r '.versions | map(.version) | sort_by(split(".") | map(tonumber)) | last // empty' "$manifest" 2>/dev/null) || cf_version=
		if [[ -z $cf_version ]]; then
			fail "$manifest names no cloudflared version"
		fi
	fi
	if [[ -f $appliance/cloudflared-versions.json ]] && ! cmp -s "$manifest" "$appliance/cloudflared-versions.json"; then
		fail "$appliance/cloudflared-versions.json differs from $manifest"
	fi

	for arch in amd64 arm64; do
		deb=pco_${version}_$arch.deb
		template=$appliance/pco-appliance_${version}_$arch.tar.zst
		# The sum build.sh wrote is a first look; the programs in the
		# template are what count.
		sums=$appliance/pco-appliance_${version}_$arch.deb.sha256
		if [[ ! -f $sums ]]; then
			fail "$appliance has no ${sums##*/}, so nothing says which package the $arch template carries"
			continue
		fi
		read -r built built_name <"$sums" || true
		if [[ ${built_name:-} != "$deb" || ! ${built:-} =~ ^[0-9a-f]{64}$ ]]; then
			fail "${sums##*/} is not one line \"<sha256>  $deb\""
			continue
		elif [[ -f $dist/$deb && $(sha256 "$dist/$deb") != "$built" ]]; then
			fail "the $arch template was built from a $deb with the sha256 $built, which is not the package of this release"
			continue
		fi
		if [[ -n $missing || ! -f $template || ! -f $dist/$deb ]]; then
			continue
		fi
		carries "$arch" pco "$template" "$dist/$deb"

		[[ -n $cf_version ]] || continue
		cf_deb=$appliance/cloudflared_${cf_version}_$arch.deb
		cf_sha256=$(jq -r --arg v "$cf_version" --arg a "$arch" '.versions[] | select(.version == $v) | .[$a].sha256 // empty' "$manifest")
		if [[ ! -f $cf_deb ]]; then
			fail "$appliance has no ${cf_deb##*/}, the cloudflared package the $arch template was built from"
		elif [[ $(sha256 "$cf_deb") != "$cf_sha256" ]]; then
			fail "${cf_deb##*/} has the sha256 $(sha256 "$cf_deb"), $manifest lists ${cf_sha256:-none}"
		else
			carries "$arch" cloudflared "$template" "$cf_deb"
		fi
	done
	pin=$appliance/pco-appliance_${version}.pin.conf
	if [[ -f $pin ]] && ! grep -Eq '^SNAPSHOT=[0-9]{8}T[0-9]{6}Z$' "$pin"; then
		fail "${pin##*/} names no snapshot"
	fi
fi

if command -v dpkg-deb >/dev/null 2>&1; then
	tilde='~'
	deb_version=${version/-/$tilde}
	units='pco.service pco-cloudflared@.service pco-egress.service pco-net.service pco-web.service'
	# Written by the hooks of goreleaser from the help of the commands.
	docs='/usr/share/man/man1/pco.1.gz /usr/share/bash-completion/completions/pco /usr/share/zsh/vendor-completions/_pco /usr/share/fish/vendor_completions.d/pco.fish'
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
		# The units the package installs, which the appliance template
		# enables, pco-net.service among them, read from its listing.
		listing=$(dpkg-deb --contents "$deb" 2>/dev/null) || listing=
		for unit in $units; do
			if ! grep -Eq "[[:space:]]\./usr/lib/systemd/system/${unit//./\\.}\$" <<<"$listing"; then
				fail "${deb##*/} does not carry /usr/lib/systemd/system/$unit"
			fi
		done
		for file in $docs; do
			if ! grep -Eq "[[:space:]]\.${file//./\\.}\$" <<<"$listing"; then
				fail "${deb##*/} does not carry $file"
			fi
		done
	done
elif [[ $require_dpkg == 1 ]]; then
	fail "dpkg-deb is required to read the control files of the packages and was not found"
else
	printf 'check-artifacts.sh: dpkg-deb not found, the control files of the packages are not checked\n' >&2
fi

# The keys pco upgrade checks a release with: the keyring each package of a
# release carries must hold the release keys of scripts/install.sh and no
# other, or the upgrade would trust another key than the installer.
if [[ $signed == 1 ]]; then
	if ! command -v dpkg-deb >/dev/null 2>&1 || ! command -v gpg >/dev/null 2>&1; then
		if [[ $require_dpkg == 1 ]]; then
			fail "dpkg-deb and gpg are required to read the release key out of the packages and were not found"
		else
			printf 'check-artifacts.sh: dpkg-deb or gpg not found, the release key in the packages is not checked\n' >&2
		fi
	else
		keys=$(mktemp -d "${TMPDIR:-/tmp}/check-artifacts-keys.XXXXXXXX")
		mkdir -m 700 "$keys/home"
		if ! want_keys=$(bash "$HERE/release-key.sh" "$HERE/../scripts/install.sh" "$keys/install.gpg" 2>"$keys/err"); then
			fail "scripts/install.sh carries no release key to compare with: $(<"$keys/err")"
		else
			for arch in amd64 arm64; do
				deb=$dist/pco_${version}_$arch.deb
				[[ -f $deb ]] || continue
				if ! dpkg-deb --fsys-tarfile "$deb" 2>/dev/null | tar -xO ./usr/share/pco/release-key.gpg >"$keys/$arch.gpg" 2>/dev/null ||
					[[ ! -s $keys/$arch.gpg ]]; then
					fail "${deb##*/} has no ./usr/share/pco/release-key.gpg"
					continue
				fi
				got_keys=$(GNUPGHOME=$keys/home gpg --batch --show-keys --with-colons "$keys/$arch.gpg" 2>/dev/null |
					awk -F: '$1 == "pub" { want = 1; next } $1 == "sub" { want = 0 } $1 == "fpr" && want { print $10; want = 0 }')
				if [[ $got_keys != "$want_keys" ]]; then
					got_keys=${got_keys//$'\n'/ }
					fail "${deb##*/} carries the release keys ${got_keys:-<none>}, scripts/install.sh carries ${want_keys//$'\n'/ }"
				fi
			done
		fi
		GNUPGHOME=$keys/home gpgconf --kill all >/dev/null 2>&1 || true
		rm -rf "$keys"
	fi
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
