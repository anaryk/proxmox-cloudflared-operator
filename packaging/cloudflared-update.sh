#!/usr/bin/env bash
#
# Adds a release of cloudflared to packaging/cloudflared-versions.json: it
# downloads the packages of both architectures from the release on GitHub,
# writes an entry with their URLs and sha256 and sets "updated" to the day.
# The cloudflared workflow runs it each week and opens a pull request with the
# change; merging that pull request is what allows the version.
#
# Usage: packaging/cloudflared-update.sh <version> [manifest]
#
# It refuses a version the manifest names already, allowed or denied, and
# leaves the manifest as it was when a download fails or, where dpkg-deb is
# installed, a package is not cloudflared of that version and architecture.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RELEASES=https://github.com/cloudflare/cloudflared/releases/download

die() {
	printf 'cloudflared-update.sh: %s\n' "$*" >&2
	exit 1
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
	printf 'usage: cloudflared-update.sh <version> [manifest]\n' >&2
	exit 2
fi
version=$1
manifest=${2:-$HERE/cloudflared-versions.json}
[[ $version =~ ^[0-9]{4}\.[0-9]{1,2}\.[0-9]+$ ]] || die "$version is not a version of cloudflared, which are YYYY.M.N"
[[ -f $manifest && -r $manifest ]] || die "not a readable file: $manifest"
if jq -e --arg v "$version" 'any(.versions[]; .version == $v)' "$manifest" >/dev/null; then
	die "$manifest allows cloudflared $version already"
fi
if jq -e --arg v "$version" 'any(.deny[]; .version == $v)' "$manifest" >/dev/null; then
	die "$manifest denies cloudflared $version"
fi

sha256() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${sum%% *}"
}

work=$(mktemp -d "${TMPDIR:-/tmp}/cloudflared-update.XXXXXXXX")
trap 'rm -rf "$work"' EXIT

args=()
for arch in amd64 arm64; do
	url=$RELEASES/$version/cloudflared-linux-$arch.deb
	deb=$work/$arch.deb
	curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
		--max-filesize 200000000 --output "$deb" "$url" || die "cannot download $url"
	if command -v dpkg-deb >/dev/null 2>&1; then
		got="$(dpkg-deb --field "$deb" Package 2>/dev/null || true) $(dpkg-deb --field "$deb" Version 2>/dev/null || true) $(dpkg-deb --field "$deb" Architecture 2>/dev/null || true)"
		[[ $got == "cloudflared $version $arch" ]] || die "$url is not cloudflared $version for $arch but '$got'"
	else
		printf 'cloudflared-update.sh: dpkg-deb not found, the control file of %s is not checked\n' "${url##*/}" >&2
	fi
	args+=(--arg "${arch}_url" "$url" --arg "${arch}_sha256" "$(sha256 "$deb")")
done

jq --arg v "$version" --arg today "$(date -u +%Y-%m-%d)" "${args[@]}" '
	.versions = ([.versions[], {
		version: $v,
		amd64: {url: $amd64_url, sha256: $amd64_sha256},
		arm64: {url: $arm64_url, sha256: $arm64_sha256}
	}] | sort_by(.version | split(".") | map(tonumber)) | reverse)
	| .updated = $today' "$manifest" >"$work/manifest.json"
cat "$work/manifest.json" >"$manifest"
jq --arg v "$version" '.versions[] | select(.version == $v)' "$manifest"
