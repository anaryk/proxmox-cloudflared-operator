#!/usr/bin/env bash
#
# Reads the release key that scripts/install.sh carries, the way the installer
# reads it, writes it to a file as a binary keyring and prints the fingerprint
# of every primary key in it, one per line.
#
# The release workflow runs it on the tagged commit, to learn whether the key
# the release is signed with is one of the keys every installer of that commit
# trusts: a release signed with any other key is refused by all of them. The
# keyring may hold two keys while one is replaced by the other.
#
# Usage: packaging/release-key.sh <install.sh> <keyring-file>
#
# It fails with one line, and leaves no keyring behind, when the script has no
# key block, still has the placeholder, assigns the variable of the key anywhere
# else, has a block the installer's base64 would refuse, or the block holds no
# OpenPGP key.

set -euo pipefail

NAME=PCO_RELEASE_KEY_B64
HEADER="read -r -d '' $NAME <<'EOF' || true"
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
# nothing must not look like an empty key. Every other line that is not a
# comment may only expand the variable. Anything else, a later assignment, a
# second read, printf -v, would make the installer use another key than the
# one the block holds.
raw=
state=before
headers=0
lineno=0
elsewhere=
expand_plain="\$$NAME"
expand_braced="\${$NAME}"
while IFS= read -r line || [[ -n $line ]]; do
	lineno=$((lineno + 1))
	if [[ $line == "$HEADER" ]]; then
		headers=$((headers + 1))
		if [[ $state == before ]]; then
			state=inside
		fi
		continue
	fi
	if [[ $state == inside ]]; then
		if [[ $line == EOF ]]; then
			state=after
		else
			raw="$raw$line"$'\n'
		fi
		continue
	fi
	if [[ $line =~ ^[[:space:]]*# ]]; then
		continue
	fi
	rest=${line//"$expand_plain"/}
	rest=${rest//"$expand_braced"/}
	if [[ $rest == *"$NAME"* && -z $elsewhere ]]; then
		elsewhere=$lineno
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
if [[ -n $elsewhere ]]; then
	die "line $elsewhere of $script uses $NAME for more than expanding it, so the installer may not use the key of the block"
fi

# The installer reads the block with read -r -d '', which strips the white space
# around the whole text and keeps the lines, and then decodes it with base64 -d.
# GNU base64 -d skips newlines, so blank lines are fine, and refuses a space, a
# tab or a CR anywhere else; other base64 tools are not as strict, so the lines
# are checked here as well.
value=
read -r -d '' value <<<"$raw" || true
if [[ -z $value ]]; then
	die "the key block of $script is empty"
fi
if [[ $value == "$PLACEHOLDER" ]]; then
	die "$script still carries the placeholder $PLACEHOLDER, paste the release key into it before tagging"
fi
while IFS= read -r line || [[ -n $line ]]; do
	if [[ -n $line && ! $line =~ ^[A-Za-z0-9+/=]+$ ]]; then
		die "the key block of $script has a line that is not plain base64, which the installer cannot decode; check for spaces, tabs and carriage returns"
	fi
done <<<"$value"

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pco-release-key.XXXXXXXX")
trap cleanup EXIT
printf '%s\n' "$value" | base64 -d >"$TMP_DIR/keyring" 2>/dev/null ||
	die "the key block of $script is not valid base64"

# gpg is given a home of its own, so that it neither reads nor creates the
# keys of whoever runs this.
chmod 700 "$TMP_DIR"
listing=$(GNUPGHOME=$TMP_DIR gpg --batch --show-keys --with-colons "$TMP_DIR/keyring" 2>/dev/null) ||
	die "the key block of $script holds no OpenPGP keyring"

# The first fpr line after a pub line is the fingerprint of that primary key;
# the fpr lines of its subkeys follow sub lines.
fprs=()
wanted=0
re='^[0-9A-F]{40}([0-9A-F]{24})?$'
while IFS= read -r line; do
	case $line in
	pub:*)
		wanted=1
		;;
	sub:*)
		wanted=0
		;;
	fpr:*)
		if [[ $wanted == 1 ]]; then
			wanted=0
			IFS=: read -r -a field <<<"$line"
			if ! [[ ${field[9]:-} =~ $re ]]; then
				die "gpg named no usable fingerprint for a key of $script"
			fi
			fprs+=("${field[9]}")
		fi
		;;
	esac
done <<<"$listing"
if [[ ${#fprs[@]} == 0 ]]; then
	die "the key block of $script holds no OpenPGP key"
fi

cp -- "$TMP_DIR/keyring" "$out" 2>/dev/null || die "cannot write $out"
printf '%s\n' "${fprs[@]}"
