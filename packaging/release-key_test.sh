#!/usr/bin/env bash
#
# Tests for release-key.sh. The scripts it reads are copies of the real
# scripts/install.sh whose key block was replaced, so that a change to the
# form of that block in install.sh breaks the tests and not the release.
# Keys are made with gpg in a throwaway home; with PCO_REQUIRE_CRYPTO_TESTS=1 a
# missing gpg fails the run instead of skipping those cases.
#
# Run it with: bash packaging/release-key_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/release-key.sh
INSTALL=$HERE/../scripts/install.sh
HEADER="read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true"
PLACEHOLDER=REPLACE-WITH-THE-RELEASE-KEY

CASE=
CHECKS=0
FAILS=0
SKIPPED=0
RC=0
OUT=
ERR=
GPG_HOME=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-release-key-test.XXXXXXXX") || exit 1

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
is_empty() { [[ -z $1 ]]; }
equals() { [[ $1 == "$2" ]]; }
one_line() { [[ $1 != *$'\n'* ]]; }
refused() { [[ $RC == 1 && $ERR == "release-key.sh: "* ]]; }

is_fingerprint() {
	local re='^[0-9A-F]{40}([0-9A-F]{24})?$'
	[[ $1 =~ $re ]]
}

# What the real install.sh may not make the script say: that it found no block.
block_found() {
	[[ $ERR != *"no line"* && $ERR != *"not closed"* && $ERR != *"key block headers"* ]]
}

# run <install.sh> [keyring]: leaves RC, OUT and ERR.
run() {
	local keyring=${2:-$ROOT/keyring}
	rm -f "$keyring"
	RC=0
	OUT=$(bash "$SUT" "$1" "$keyring" 2>"$ROOT/err") || RC=$?
	ERR=$(<"$ROOT/err")
}

# variant <out> <key block text> [header]: a copy of install.sh with the lines
# between the header and the closing EOF replaced. The copy fails loudly when
# install.sh no longer has exactly one such block.
variant() {
	local out=$1 key=$2 header=${3:-$HEADER} line state=before found=0
	while IFS= read -r line; do
		if [[ $state == inside ]]; then
			if [[ $line == EOF ]]; then
				state=after
				printf '%s\n' "$line"
			fi
		elif [[ $line == "$HEADER" ]]; then
			printf '%s\n' "$header"
			if [[ -n $key ]]; then
				printf '%s\n' "$key"
			fi
			state=inside
			found=$((found + 1))
		else
			printf '%s\n' "$line"
		fi
	done <"$INSTALL" >"$out"
	if [[ $found != 1 || $state != after ]]; then
		printf 'release-key_test.sh: scripts/install.sh has %s key blocks, expected one closed by EOF\n' "$found" >&2
		exit 1
	fi
}

gpg_cmd() {
	GNUPGHOME=$GPG_HOME gpg --batch --quiet "$@"
}

fingerprint() {
	gpg_cmd --with-colons --fingerprint "$1" | awk -F: '$1 == "fpr" { print $10; exit }'
}

CASE='the real scripts/install.sh'
run "$INSTALL"
assert "its key block is found" block_found
if [[ $RC == 0 ]]; then
	assert "a key in it gives a fingerprint" is_fingerprint "$OUT"
else
	assert "the only refusal it may give is the placeholder" contains "$ERR" "still carries the placeholder"
fi

CASE='the placeholder'
variant "$ROOT/placeholder.sh" "$PLACEHOLDER"
printf 'old' >"$ROOT/old"
RC=0
OUT=$(bash "$SUT" "$ROOT/placeholder.sh" "$ROOT/old" 2>"$ROOT/err") || RC=$?
ERR=$(<"$ROOT/err")
assert "is refused" refused
assert "is named in one line" contains "$ERR" "still carries the placeholder $PLACEHOLDER"
assert "in one line only" one_line "$ERR"
assert "prints nothing on stdout" is_empty "$OUT"
assert "leaves a keyring that was there alone" equals "$(<"$ROOT/old")" old

CASE='no key block'
variant "$ROOT/changed.sh" abcd "read -r -d \"\" PCO_RELEASE_KEY_B64 <<'EOF' || true"
run "$ROOT/changed.sh"
assert "a changed header line is refused, not read as an empty key" refused
assert "and the refusal says what is missing" contains "$ERR" "has no line"
assert "and no keyring is written" test ! -e "$ROOT/keyring"
printf '#!/bin/bash\necho hello\n' >"$ROOT/plain.sh"
run "$ROOT/plain.sh"
assert "a script without the block is refused" refused
assert "and the refusal says what is missing" contains "$ERR" "has no line"
run "$ROOT/missing.sh"
assert "a missing file is refused" refused
assert "and the refusal says so" contains "$ERR" "not a readable file"

CASE='a damaged block'
variant "$ROOT/empty.sh" ''
run "$ROOT/empty.sh"
assert "an empty block is refused" refused
assert "as empty" contains "$ERR" "is empty"
printf '%s\n%s\n' "$HEADER" AAAA >"$ROOT/open.sh"
run "$ROOT/open.sh"
assert "a block without its closing EOF is refused" refused
assert "as not closed" contains "$ERR" "not closed"
printf '%s\nAAAA\nEOF\n%s\nBBBB\nEOF\n' "$HEADER" "$HEADER" >"$ROOT/twice.sh"
run "$ROOT/twice.sh"
assert "two blocks are refused" refused
assert "as two" contains "$ERR" "has 2 key block headers"
variant "$ROOT/notb64.sh" '%%%% not base64 %%%%'
run "$ROOT/notb64.sh"
assert "text that is not base64 is refused" refused
assert "and no keyring is written" test ! -e "$ROOT/keyring"
variant "$ROOT/notkey.sh" "$(printf 'fake-keyring-bytes' | base64 | tr -d '\n')"
run "$ROOT/notkey.sh"
assert "base64 that is not a key is refused" refused
assert "and no keyring is written" test ! -e "$ROOT/keyring"

CASE='usage'
RC=0
OUT=$(bash "$SUT" one 2>"$ROOT/err") || RC=$?
ERR=$(<"$ROOT/err")
assert "one argument is exit status 2" rc_is 2
assert "and the usage" contains "$ERR" "usage: release-key.sh"

crypto_cases() {
	local fpr_a fpr_b
	if ! command -v gpg >/dev/null 2>&1; then
		if [[ ${PCO_REQUIRE_CRYPTO_TESTS:-} == 1 ]]; then
			CASE='the cases with real keys'
			RC=1
			OUT=
			ERR=
			assert "PCO_REQUIRE_CRYPTO_TESTS=1 but gpg is not installed" false
		else
			SKIPPED=$((SKIPPED + 1))
			printf 'skipped [the cases with real keys]: gpg is not installed\n'
		fi
		return
	fi
	# A short path: the socket of gpg-agent must fit into the limit of the system.
	GPG_HOME=$(mktemp -d /tmp/pco-rk-gpg.XXXXXX)
	chmod 700 "$GPG_HOME"
	gpg_cmd --passphrase '' --quick-generate-key 'Release Test <release@example.invalid>' ed25519 sign never 2>/dev/null
	fpr_a=$(fingerprint release@example.invalid)
	gpg_cmd --passphrase '' --quick-add-key "$fpr_a" ed25519 sign never 2>/dev/null
	gpg_cmd --passphrase '' --quick-generate-key 'Other Test <other@example.invalid>' ed25519 sign never 2>/dev/null
	fpr_b=$(fingerprint other@example.invalid)
	gpg_cmd --export "$fpr_a" >"$ROOT/a.gpg"
	gpg_cmd --export "$fpr_a" "$fpr_b" >"$ROOT/ab.gpg"

	CASE='a key with a subkey'
	variant "$ROOT/keyed.sh" "$(base64 <"$ROOT/a.gpg" | tr -d '\n' | fold -w 64)"
	run "$ROOT/keyed.sh"
	assert "is accepted, wrapped as it is pasted by hand" rc_is 0
	assert "prints the fingerprint of the primary key and nothing else" equals "$OUT" "$fpr_a"
	assert "writes the keyring the installer would decode" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"
	variant "$ROOT/keyed1.sh" "$(base64 <"$ROOT/a.gpg" | tr -d '\n')"
	run "$ROOT/keyed1.sh"
	assert "is accepted on one line too" rc_is 0
	assert "with the same fingerprint" equals "$OUT" "$fpr_a"
	assert "and the same keyring" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"

	CASE='the home of gpg'
	mkdir "$ROOT/home"
	RC=0
	OUT=$(GNUPGHOME=$ROOT/home bash "$SUT" "$ROOT/keyed.sh" "$ROOT/keyring" 2>"$ROOT/err") || RC=$?
	ERR=$(<"$ROOT/err")
	assert "of the caller is not used" rc_is 0
	assert "and gets no files" is_empty "$(ls -A "$ROOT/home")"

	CASE='two keys'
	variant "$ROOT/two.sh" "$(base64 <"$ROOT/ab.gpg" | tr -d '\n' | fold -w 64)"
	run "$ROOT/two.sh"
	assert "are refused, the signing key could not be told from the other" refused
	assert "with their number" contains "$ERR" "holds 2 OpenPGP keys, expected one"
	assert "and no keyring is written" test ! -e "$ROOT/keyring"
}
crypto_cases

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
