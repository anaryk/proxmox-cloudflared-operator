#!/usr/bin/env bash
#
# Builds the appliance template: a Debian 13 root file system that mmdebstrap
# makes from the snapshot of snapshot.debian.org that pin.conf names, with pco
# and cloudflared installed and the overlay copied on. Two builds from the same
# snapshot and the same packages give the same files, byte for byte.
#
# Usage: packaging/appliance/build.sh --arch ARCH --version VERSION --deb FILE
#            [--snapshot TIMESTAMP] [--out DIR] [--cache DIR] [--dry-run]
#
#   --arch      amd64 or arm64; the one the host is not needs qemu-user-static
#               and binfmt.
#   --version   the version of pco as the names of its packages carry it,
#               0.1.0 or 0.1.0-rc.1. It names the files.
#   --deb       the pco package to install, of that version and architecture.
#   --snapshot  a snapshot.debian.org timestamp, YYYYMMDDTHHMMSSZ, to build
#               from instead of the one in pin.conf.
#   --out       where the files go, build/appliance unless given.
#   --cache     where the cloudflared package is downloaded to and kept; a
#               temporary directory unless given.
#   --dry-run   print what would run and stop, without downloading anything.
#
# It writes into the out directory:
#
#   pco-appliance_<version>_<arch>.tar.zst     the template
#   pco-appliance_<version>_<arch>.spdx.json   its packages, as SPDX 2.3
#   pco-appliance_<version>_<arch>.deb.sha256  the sha256 of the pco package
#                                              it installed; the release
#                                              refuses a package that differs
#   pco-appliance_<version>.pin.conf           the snapshot it was built from
#   cloudflared-versions.json                  the list cloudflared came from
#
# The newest version that cloudflared-versions.json allows is the cloudflared
# the template carries; its package is checked against the sha256 listed there.
# mmdebstrap runs in unshare mode: as root, or as a user with a range in
# /etc/subuid and /etc/subgid where the kernel lets users make user namespaces.
# The build itself needs Linux; README.md says how to run it elsewhere.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
MANIFEST=$HERE/../cloudflared-versions.json
REPOSITORY=https://github.com/anaryk/proxmox-cloudflared-operator

usage() {
	printf 'usage: build.sh --arch amd64|arm64 --version VERSION --deb FILE [--snapshot TIMESTAMP] [--out DIR] [--cache DIR] [--dry-run]\n' >&2
	exit 2
}

die() {
	printf 'build.sh: %s\n' "$*" >&2
	exit 1
}

say() {
	printf 'build.sh: %s\n' "$*" >&2
}

arch=
version=
deb=
snapshot=
out=build/appliance
cache=
dry=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--dry-run) dry=1 ;;
	--arch | --version | --deb | --snapshot | --out | --cache)
		[[ $# -ge 2 && -n $2 ]] || usage
		case $1 in
		--arch) arch=$2 ;;
		--version) version=$2 ;;
		--deb) deb=$2 ;;
		--snapshot) snapshot=$2 ;;
		--out) out=$2 ;;
		--cache) cache=$2 ;;
		esac
		shift
		;;
	*) usage ;;
	esac
	shift
done
[[ -n $arch && -n $version && -n $deb ]] || usage
case $arch in
amd64 | arm64) ;;
*) die "the architecture is amd64 or arm64, not $arch" ;;
esac
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z.+~-]*$ ]] || die "$version is not a version of pco"
[[ -f $deb && -r $deb ]] || die "not a readable file: $deb"

sha256() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${sum%% *}"
}

# pin <file>: the value of the SNAPSHOT line, without running the file.
pin() {
	local line value=
	while IFS= read -r line || [[ -n $line ]]; do
		case $line in
		SNAPSHOT=*) value=${line#SNAPSHOT=} ;;
		esac
	done <"$1"
	printf '%s\n' "$value"
}

if [[ -z $snapshot ]]; then
	snapshot=$(pin "$HERE/pin.conf")
fi
if ! [[ $snapshot =~ ^([0-9]{4})([0-9]{2})([0-9]{2})T([0-9]{2})([0-9]{2})([0-9]{2})Z$ ]]; then
	die "the snapshot is a timestamp YYYYMMDDTHHMMSSZ, not '$snapshot'"
fi
iso="${BASH_REMATCH[1]}-${BASH_REMATCH[2]}-${BASH_REMATCH[3]}T${BASH_REMATCH[4]}:${BASH_REMATCH[5]}:${BASH_REMATCH[6]}Z"
# GNU date, then the BSD one for a dry run on macOS.
epoch=$(date -u -d "$iso" +%s 2>/dev/null || date -j -u -f '%Y%m%dT%H%M%SZ' "$snapshot" +%s 2>/dev/null) ||
	die "$snapshot is not a time"
# snapshot.debian.org answers a time to come with what it has now, which a
# later build would not get again.
if ((epoch > $(date +%s))); then
	die "the snapshot $snapshot is in the future"
fi

include=
while IFS= read -r line || [[ -n $line ]]; do
	line=${line%%#*}
	line=${line//[[:space:]]/}
	[[ -n $line ]] || continue
	include=${include:+$include,}$line
done <"$HERE/packages.txt"

# The newest version the manifest allows, and its package for this architecture.
cf_version=$(jq -r '.versions | map(.version) | sort_by(split(".") | map(tonumber)) | last // empty' "$MANIFEST") ||
	die "$MANIFEST cannot be read"
[[ -n $cf_version ]] || die "$MANIFEST allows no version of cloudflared"
if jq -e --arg v "$cf_version" 'any(.deny[]; .version == $v)' "$MANIFEST" >/dev/null; then
	die "$MANIFEST allows and denies cloudflared $cf_version"
fi
cf_url=$(jq -r --arg v "$cf_version" --arg a "$arch" '.versions[] | select(.version == $v) | .[$a].url // empty' "$MANIFEST")
cf_sha256=$(jq -r --arg v "$cf_version" --arg a "$arch" '.versions[] | select(.version == $v) | .[$a].sha256 // empty' "$MANIFEST")
[[ $cf_url == https://* ]] || die "$MANIFEST has no https URL for cloudflared $cf_version on $arch"
[[ $cf_sha256 =~ ^[0-9a-f]{64}$ ]] || die "$MANIFEST has no sha256 for cloudflared $cf_version on $arch"

name=pco-appliance_${version}_$arch

# The customize hooks, in this order. The overlay goes over what Debian's
# packages installed, before pco and cloudflared, which ship none of its
# files. The last hook takes out what only the build needed: the snapshot
# sources and the apt option that lets apt read their old Release files, so
# that apt in the template reads the live archives of debian.sources only,
# the resolver of the host and the logs of the build. The package lists, the
# caches of apt and /tmp are emptied by mmdebstrap itself after the hooks: it
# still needs them until then. mmdebstrap runs each hook with sh and the root
# of the template as $1, which is why they are in single quotes; in unshare
# mode the directory it was started in may be closed to the hook.
# shellcheck disable=SC2016
finish='set -e
cd /
rm -f "$1/etc/apt/apt.conf.d/99mmdebstrap" "$1/etc/apt/sources.list" "$1"/etc/apt/sources.list.d/0000* "$1/etc/resolv.conf"
rm -f "$1/tmp/pco.deb" "$1/tmp/cloudflared.deb"
rm -rf "$1"/var/tmp/*
find "$1/var/log" -type f -exec truncate --size=0 {} +'

tmp=${TMPDIR:-/tmp}
tmp=${tmp%/}
work=$tmp/pco-appliance-build.dry
# shellcheck disable=SC2016
hooks() {
	printf '%s\n' \
		"tar-in $work/overlay.tar /" \
		"upload $work/pco.deb /tmp/pco.deb" \
		"upload $work/cloudflared.deb /tmp/cloudflared.deb" \
		'chroot "$1" dpkg --install /tmp/pco.deb /tmp/cloudflared.deb' \
		'chroot "$1" systemctl enable pco-egress.service pco.service pco-first-boot.service unattended-upgrades.service' \
		'chroot "$1" systemctl mask nftables.service' \
		'chroot "$1" apt-mark hold pco cloudflared' \
		'chroot "$1" passwd --lock root' \
		': >"$1/etc/machine-id"'
}

mmdebstrap_cmd() {
	local hook
	cmd=(mmdebstrap --mode=unshare --variant=minbase --format=tar "--architectures=$arch"
		"--include=$include" '--aptopt=Acquire::Check-Valid-Until "false"')
	while IFS= read -r hook; do
		cmd+=("--customize-hook=$hook")
	done < <(hooks)
	cmd+=("--customize-hook=$finish" trixie "$work/rootfs.tar"
		"deb http://snapshot.debian.org/archive/debian/$snapshot/ trixie main"
		"deb http://snapshot.debian.org/archive/debian-security/$snapshot/ trixie-security main")
}

if [[ $dry == 1 ]]; then
	mmdebstrap_cmd
	printf 'cloudflared %s for %s from %s, sha256 %s\n' "$cf_version" "$arch" "$cf_url" "$cf_sha256"
	printf 'SOURCE_DATE_EPOCH=%s' "$epoch"
	printf ' %q' "${cmd[@]}"
	printf '\n'
	printf 'writes %s\n' "$out/$name.tar.zst" "$out/$name.spdx.json" "$out/$name.deb.sha256" \
		"$out/pco-appliance_$version.pin.conf" "$out/cloudflared-versions.json"
	exit 0
fi

[[ $(uname -s) == Linux ]] || die "the template is built on Linux; packaging/appliance/README.md says how to build it elsewhere"
for tool in mmdebstrap zstd dpkg-deb dpkg-query curl jq; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and was not found"
done

work=$(mktemp -d "$tmp/pco-appliance-build.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
# The special hooks of mmdebstrap split their arguments at white space.
[[ $work != *[[:space:]]* ]] || die "the temporary directory $work has white space in its path"
cache=${cache:-$work/cache}
mkdir -p "$cache" "$work/out"

# field <deb> <name>: one field of the control file of a package.
field() {
	dpkg-deb --field "$1" "$2" 2>/dev/null || true
}

# The control file says 0.1.0~rc.1 where the file name says 0.1.0-rc.1.
tilde='~'
want_version=${version/-/$tilde}
if [[ $(field "$deb" Package) != pco || $(field "$deb" Version) != "$want_version" || $(field "$deb" Architecture) != "$arch" ]]; then
	die "$deb is not pco $want_version for $arch"
fi
cp "$deb" "$work/pco.deb"

cf_deb=$cache/cloudflared_${cf_version}_$arch.deb
if [[ ! -f $cf_deb || $(sha256 "$cf_deb") != "$cf_sha256" ]]; then
	say "downloading cloudflared $cf_version for $arch"
	curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
		--output "$cf_deb.part" "$cf_url" || die "cannot download $cf_url"
	got=$(sha256 "$cf_deb.part")
	if [[ $got != "$cf_sha256" ]]; then
		rm -f "$cf_deb.part"
		die "$cf_url has the sha256 $got, $MANIFEST lists $cf_sha256"
	fi
	mv "$cf_deb.part" "$cf_deb"
fi
if [[ $(field "$cf_deb" Package) != cloudflared || $(field "$cf_deb" Version) != "$cf_version" || $(field "$cf_deb" Architecture) != "$arch" ]]; then
	die "$cf_url is not cloudflared $cf_version for $arch"
fi
cp "$cf_deb" "$work/cloudflared.deb"

# The overlay as root's, with the mtime of the snapshot and modes that do not
# depend on the umask of the checkout.
tar --create --file "$work/overlay.tar" --directory "$HERE/overlay" \
	--sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
	--mode='u=rwX,go=rX' --format=pax --pax-option='delete=atime,delete=ctime' etc

say "building $name from the Debian snapshot $snapshot"
mmdebstrap_cmd
SOURCE_DATE_EPOCH=$epoch "${cmd[@]}"

say "compressing $name.tar.zst"
zstd -19 --long=27 -T0 --quiet --force "$work/rootfs.tar" -o "$work/out/$name.tar.zst"

# The packages of the template, read from its dpkg database by the dpkg of the
# host, which does not need to run anything of the other architecture.
mkdir -p "$work/dpkg/updates" "$work/dpkg/info"
tar --extract --to-stdout --file "$work/rootfs.tar" ./var/lib/dpkg/status >"$work/dpkg/status"
dpkg-query "--admindir=$work/dpkg" --show \
	--showformat='${db:Status-Status}\t${Package}\t${Version}\t${Architecture}\n' |
	awk -F'\t' '$1 == "installed" { print $2 "\t" $3 "\t" $4 }' | LC_ALL=C sort >"$work/packages.tsv"
jq --null-input --sort-keys \
	--arg name "$name" \
	--arg version "$version" \
	--arg created "$iso" \
	--arg namespace "$REPOSITORY/spdx/$name" \
	--arg download "$REPOSITORY/releases/download/v$version/$name.tar.zst" \
	--rawfile list "$work/packages.tsv" '
	def id: "SPDXRef-Package-deb-" + gsub("\\+"; "plus");
	def purl:
		if .name == "pco" then "pkg:github/anaryk/proxmox-cloudflared-operator@v" + $version
		elif .name == "cloudflared" then "pkg:github/cloudflare/cloudflared@" + .version
		else "pkg:deb/debian/" + (.name | @uri) + "@" + (.version | @uri) + "?arch=" + .arch + "&distro=trixie"
		end;
	($list | split("\n") | map(select(. != "") | split("\t") | {name: .[0], version: .[1], arch: .[2]})) as $packages
	| {
		spdxVersion: "SPDX-2.3",
		dataLicense: "CC0-1.0",
		SPDXID: "SPDXRef-DOCUMENT",
		name: $name,
		documentNamespace: $namespace,
		creationInfo: {created: $created, creators: ["Tool: pco packaging/appliance/build.sh"]},
		packages: ([{
			name: "pco-appliance",
			SPDXID: "SPDXRef-pco-appliance",
			versionInfo: $version,
			downloadLocation: $download,
			filesAnalyzed: false,
			primaryPackagePurpose: "CONTAINER"
		}] + ($packages | map({
			name,
			SPDXID: (.name | id),
			versionInfo: .version,
			downloadLocation: "NOASSERTION",
			filesAnalyzed: false,
			externalRefs: [{referenceCategory: "PACKAGE-MANAGER", referenceType: "purl", referenceLocator: purl}]
		}))),
		relationships: ([{spdxElementId: "SPDXRef-DOCUMENT", relationshipType: "DESCRIBES", relatedSpdxElement: "SPDXRef-pco-appliance"}]
			+ ($packages | map({spdxElementId: "SPDXRef-pco-appliance", relationshipType: "CONTAINS", relatedSpdxElement: (.name | id)})))
	}' >"$work/out/$name.spdx.json"

printf '%s  pco_%s_%s.deb\n' "$(sha256 "$work/pco.deb")" "$version" "$arch" >"$work/out/$name.deb.sha256"
printf '# The snapshot of snapshot.debian.org the templates of pco %s were built from.\nSNAPSHOT=%s\n' \
	"$version" "$snapshot" >"$work/out/pco-appliance_$version.pin.conf"
cp "$MANIFEST" "$work/out/cloudflared-versions.json"

# The files both architectures write must not differ between them: one
# release has one snapshot and one manifest.
mkdir -p "$out"
for shared in "pco-appliance_$version.pin.conf" cloudflared-versions.json; do
	if [[ -f $out/$shared ]] && ! cmp -s "$out/$shared" "$work/out/$shared"; then
		die "$out/$shared is there from another build and differs from this one; remove it or build into another directory"
	fi
done
mv "$work/out/"* "$out/"
say "wrote $out/$name.tar.zst"
