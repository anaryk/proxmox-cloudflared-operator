#!/usr/bin/env bash
#
# Tests for release-key-file.sh. The scripts it reads are copies of the real
# scripts/install.sh with their key block replaced, as in release-key_test.sh.
# The cases that read keys need gpg; with PCO_REQUIRE_CRYPTO_TESTS=1 a missing
# gpg fails the run instead of skipping them.
#
# Run it with: bash packaging/release-key-file_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/release-key-file.sh
INSTALL=$HERE/../scripts/install.sh
HEADER="read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true"
PLACEHOLDER=REPLACE-WITH-THE-RELEASE-KEY
TEST_FPR=62FD5D9D1E944F96940FB17ED1F3BF094E779032

CASE=
CHECKS=0
FAILS=0
SKIPPED=0
RC=0
OUT=
ERR=
GPG_HOME=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-release-key-file-test.XXXXXXXX") || exit 1

cleanup() {
	if [[ -n $GPG_HOME ]]; then
		GNUPGHOME=$GPG_HOME gpgconf --kill all >/dev/null 2>&1
		rm -rf "$GPG_HOME"
	fi
	rm -rf "$ROOT"
}
trap cleanup EXIT

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
equals() { [[ $1 == "$2" ]]; }
is_empty() { [[ -z $1 ]]; }

# run <arguments...>: leaves RC, OUT and ERR.
run() {
	RC=0
	OUT=$(bash "$SUT" "$@" 2>"$ROOT/err") || RC=$?
	ERR=$(<"$ROOT/err")
}

# variant <out> <key block text>: a copy of install.sh with its key block
# replaced.
variant() {
	local out=$1 key=$2 line state=before found=0
	while IFS= read -r line; do
		if [[ $state == inside ]]; then
			if [[ $line == EOF ]]; then
				state=after
				printf '%s\n' "$line"
			fi
		elif [[ $line == "$HEADER" ]]; then
			printf '%s\n%s\n' "$line" "$key"
			state=inside
			found=$((found + 1))
		else
			printf '%s\n' "$line"
		fi
	done <"$INSTALL" >"$out"
	if [[ $found != 1 || $state != after ]]; then
		printf 'release-key-file_test.sh: scripts/install.sh has %s key blocks, expected one closed by EOF\n' "$found" >&2
		exit 1
	fi
}

# The fingerprint of every primary key of a keyring, one per line.
primary_fingerprints() {
	GNUPGHOME=$GPG_HOME gpg --batch --show-keys --with-colons "$1" 2>/dev/null |
		awk -F: '$1 == "pub" { want = 1; next } $1 == "sub" { want = 0 } $1 == "fpr" && want { print $10; want = 0 }'
}

# Without gpg a key gives itself away by its first byte: an OpenPGP public key
# packet in the old format, 0x98, or in the new one, 0xc6.
starts_as_key() {
	local byte
	byte=$(od -An -tx1 -N1 "$1" | tr -d ' \n')
	[[ $byte == 98 || $byte == c6 ]]
}

has_gpg() {
	if command -v gpg >/dev/null 2>&1; then
		return 0
	fi
	if [[ ${PCO_REQUIRE_CRYPTO_TESTS:-} == 1 ]]; then
		CHECKS=$((CHECKS + 1))
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] PCO_REQUIRE_CRYPTO_TESTS=1 but gpg is not installed\n' "$CASE"
	else
		SKIPPED=$((SKIPPED + 1))
		printf 'skipped [%s]: gpg is not installed\n' "$CASE"
	fi
	return 1
}

if command -v gpg >/dev/null 2>&1; then
	GPG_HOME=$(mktemp -d /tmp/pco-rkf-gpg.XXXXXX)
	chmod 700 "$GPG_HOME"
fi

CASE='usage'
run
assert "no argument is exit status 2" rc_is 2
assert "and the usage" contains "$ERR" "usage: release-key-file.sh"
run "$INSTALL"
assert "one argument is exit status 2" rc_is 2
run --snapshot --snapshot
assert "--snapshot as the keyring is exit status 2" rc_is 2

CASE='a release from a script with the placeholder'
variant "$ROOT/placeholder.sh" "$PLACEHOLDER"
printf 'a keyring of an earlier build' >"$ROOT/keyring.gpg"
run "$ROOT/placeholder.sh" "$ROOT/keyring.gpg"
assert "is refused" rc_is 1
assert "as release-key.sh refuses it" contains "$ERR" "release-key.sh: $ROOT/placeholder.sh still carries the placeholder $PLACEHOLDER"
assert "and leaves no keyring, not even the one of an earlier build" test ! -e "$ROOT/keyring.gpg"
assert "and prints nothing on stdout" is_empty "$OUT"

CASE='a snapshot'
mkdir "$ROOT/snap"
printf 'a keyring of an earlier build' >"$ROOT/snap/keyring.gpg"
run --snapshot "$ROOT/snap/keyring.gpg"
assert "is written" rc_is 0
assert "says it is the test key" equals "$OUT" "$ROOT/snap/keyring.gpg: the test key $TEST_FPR, for a snapshot"
assert "replaces the keyring of an earlier build with a key" starts_as_key "$ROOT/snap/keyring.gpg"
assert "and leaves no temporary file" equals "$(ls -A "$ROOT/snap")" keyring.gpg
run --snapshot "$ROOT/new/dir/keyring.gpg"
assert "makes the directory of the keyring" rc_is 0
assert "and writes the keyring there" test -s "$ROOT/new/dir/keyring.gpg"
assert "the same test key every time" cmp -s "$ROOT/snap/keyring.gpg" "$ROOT/new/dir/keyring.gpg"
if has_gpg; then
	assert "holds the test key and no other" equals "$(primary_fingerprints "$ROOT/snap/keyring.gpg")" "$TEST_FPR"
fi

CASE='a release from a script with a key'
if has_gpg; then
	GNUPGHOME=$GPG_HOME gpg --batch --quiet --passphrase '' --quick-generate-key 'Release Test <release@example.invalid>' ed25519 sign never 2>/dev/null
	fpr=$(GNUPGHOME=$GPG_HOME gpg --batch --with-colons --fingerprint release@example.invalid 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')
	GNUPGHOME=$GPG_HOME gpg --batch --export "$fpr" >"$ROOT/release.gpg"
	variant "$ROOT/keyed.sh" "$(base64 <"$ROOT/release.gpg" | tr -d '\n' | fold -w 64)"
	run "$ROOT/keyed.sh" "$ROOT/out/release-key.gpg"
	assert "is written" rc_is 0
	assert "as the keyring of the script" cmp -s "$ROOT/out/release-key.gpg" "$ROOT/release.gpg"
	assert "and names its keys" equals "$OUT" "$ROOT/out/release-key.gpg: the release keys of $ROOT/keyed.sh: $fpr"

	CASE='the real scripts/install.sh'
	run "$INSTALL" "$ROOT/real.gpg"
	if [[ $RC == 0 ]]; then
		fprs=$(primary_fingerprints "$ROOT/real.gpg")
		assert "gives the keyring of its block" equals "$OUT" "$ROOT/real.gpg: the release keys of $INSTALL: ${fprs//$'\n'/ }"
		assert "which is not the test key" test "$fprs" != "$TEST_FPR"
	else
		assert "the only refusal it may give is the placeholder" contains "$ERR" "still carries the placeholder"
	fi
fi

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
