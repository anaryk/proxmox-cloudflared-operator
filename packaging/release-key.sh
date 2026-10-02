#!/usr/bin/env bash
#
# Reads the release key that scripts/install.sh carries, writes it to a file as
# a binary keyring, as the installer does before it verifies a signature, and
# prints the fingerprint of its primary key.
#
# The release workflow runs it on the tagged commit, to learn whether the key
# the release is signed with is the key every installer of that commit trusts:
# a release signed with any other key is refused by all of them.
#
# Usage: packaging/release-key.sh <install.sh> <keyring-file>
#
# It fails with one line, and leaves no keyring behind, when the script has no
# key block, still has the placeholder, or carries anything but one OpenPGP key.

set -euo pipefail

HEADER="read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true"
PLACEHOLDER=REPLACE-WITH-THE-RELEASE-KEY
TMP_DIR=

die() {
	printf 'release-key.sh: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [[ -n $TMP_DIR ]]; then
		rm -rf -- "$TMP_DIR"
	fi
}

if [[ $# != 2 ]]; then
	printf 'usage: release-key.sh <install.sh> <keyring-file>\n' >&2
	exit 2
fi
script=$1
out=$2
[[ -f $script && -r $script ]] || die "not a readable file: $script"

# The block is whatever stands between the header line and the next line that
# is EOF. A header that is not found, or found twice, is an error: reading
# nothing must not look like an empty key.
b64=
state=before
headers=0
while IFS= read -r line || [[ -n $line ]]; do
	if [[ $line == "$HEADER" ]]; then
		headers=$((headers + 1))
		if [[ $state == before ]]; then
			state=inside
		fi
	elif [[ $state == inside ]]; then
		if [[ $line == EOF ]]; then
			state=after
		else
			b64="$b64${line//[[:space:]]/}"
		fi
	fi
done <"$script"
if [[ $headers == 0 ]]; then
	die "$script has no line '$HEADER', so there is no key block to read"
fi
if [[ $headers != 1 ]]; then
	die "$script has $headers key block headers, expected one"
fi
if [[ $state != after ]]; then
	die "the key block of $script is not closed by a line of EOF"
fi
if [[ -z $b64 ]]; then
	die "the key block of $script is empty"
fi
if [[ $b64 == "$PLACEHOLDER" ]]; then
	die "$script still carries the placeholder $PLACEHOLDER, paste the release key into it before tagging"
fi

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pco-release-key.XXXXXXXX")
trap cleanup EXIT
printf '%s' "$b64" | base64 -d >"$TMP_DIR/keyring" 2>/dev/null ||
	die "the key block of $script is not valid base64"

# gpg is given a home of its own, so that it neither reads nor creates the
# keys of whoever runs this.
chmod 700 "$TMP_DIR"
listing=$(GNUPGHOME=$TMP_DIR gpg --batch --show-keys --with-colons "$TMP_DIR/keyring" 2>/dev/null) ||
	die "the key block of $script holds no OpenPGP keyring"

primaries=0
fpr=
while IFS= read -r line; do
	case $line in
	pub:*)
		primaries=$((primaries + 1))
		;;
	fpr:*)
		if [[ $primaries == 1 && -z $fpr ]]; then
			IFS=: read -r -a field <<<"$line"
			fpr=${field[9]:-}
		fi
		;;
	esac
done <<<"$listing"
if [[ $primaries != 1 ]]; then
	die "the key block of $script holds $primaries OpenPGP keys, expected one"
fi
re='^[0-9A-F]{40}([0-9A-F]{24})?$'
if ! [[ $fpr =~ $re ]]; then
	die "gpg named no usable fingerprint for the key of $script"
fi

cp -- "$TMP_DIR/keyring" "$out" 2>/dev/null || die "cannot write $out"
printf '%s\n' "$fpr"
