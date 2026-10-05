#!/usr/bin/env bash
#
# Builds the appliance template twice, from the same snapshot and the same
# package, and fails unless the two builds wrote the same files, byte for
# byte. It takes the options of build.sh; the files of the first build are
# left in --out, build/appliance unless given, those of the second removed.
# Both builds download from snapshot.debian.org, so it needs what build.sh
# needs, and the time of two builds.
#
# Usage: packaging/appliance/build_test.sh --arch ARCH --version VERSION --deb FILE
#            [--snapshot TIMESTAMP] [--out DIR] [--cache DIR]

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

out=build/appliance
cache=
args=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--out | --cache)
		[[ $# -ge 2 && -n $2 ]] || {
			printf 'usage: build_test.sh <the options of build.sh>\n' >&2
			exit 2
		}
		if [[ $1 == --out ]]; then
			out=$2
		else
			cache=$2
		fi
		shift
		;;
	--dry-run)
		printf 'build_test.sh: a dry run builds nothing to compare\n' >&2
		exit 2
		;;
	*) args+=("$1") ;;
	esac
	shift
done

sha256() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${sum%% *}"
}

work=$(mktemp -d "${TMPDIR:-/tmp}/pco-appliance-twice.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
# One download of cloudflared for both.
cache=${cache:-$work/cache}

"$HERE/build.sh" "${args[@]}" --out "$work/first" --cache "$cache"
"$HERE/build.sh" "${args[@]}" --out "$work/second" --cache "$cache"

same=1
for path in "$work/first"/*; do
	name=${path##*/}
	first=$(sha256 "$path")
	second=missing
	if [[ -f $work/second/$name ]]; then
		second=$(sha256 "$work/second/$name")
	fi
	if [[ $first == "$second" ]]; then
		printf 'same       %s  %s\n' "$first" "$name"
	else
		printf 'DIFFERENT  %s  %s\n           %s  (second build)\n' "$first" "$name" "$second"
		same=0
	fi
done
if [[ $same == 0 ]]; then
	printf 'build_test.sh: the two builds differ; diffoscope shows where\n' >&2
	exit 1
fi

mkdir -p "$out"
for path in "$work/first"/*; do
	name=${path##*/}
	if [[ -f $out/$name ]] && ! cmp -s "$out/$name" "$path"; then
		case $name in
		*.pin.conf | cloudflared-versions.json)
			printf 'build_test.sh: %s/%s is there from another build and differs from this one\n' "$out" "$name" >&2
			exit 1
			;;
		esac
	fi
done
mv "$work/first/"* "$out/"
printf 'build_test.sh: both builds are the same; the files are in %s\n' "$out"
