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
NL=$'\n'
TAB=$'\t'
CR=$'\r'

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

# variant <out> <key block text> [header] [extra lines after the script]: a copy
# of install.sh with the lines between the header and the closing EOF replaced.
# The copy fails loudly when install.sh no longer has exactly one such block.
variant() {
	local out=$1 key=$2 header=${3:-$HEADER} extra=${4:-} line state=before found=0
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
	if [[ -n $extra ]]; then
		printf '%s\n' "$extra" >>"$out"
	fi
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

wrapped() {
	base64 <"$1" | tr -d '\n' | fold -w 64
}

CASE='the real scripts/install.sh'
run "$INSTALL"
assert "its key block is found" block_found
if [[ $RC == 0 ]]; then
	assert "a key in it gives fingerprints" is_fingerprint "${OUT%%$'\n'*}"
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
variant "$ROOT/placeholder-around.sh" "${NL}  $PLACEHOLDER  ${NL}"
run "$ROOT/placeholder-around.sh"
assert "is still the placeholder with white space around it, as the installer sees it" contains "$ERR" "still carries the placeholder"

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

# What GNU base64 -d, which the installer runs, refuses once read -r -d '' has
# stripped the white space around the whole block: any space, tab or CR left
# inside it. Another base64 would let them through, which is why the script
# looks at the lines itself.
CASE='white space the installer cannot decode'
variant "$ROOT/trailing-space.sh" "AAAA ${NL}BBBB${NL}CCCC"
run "$ROOT/trailing-space.sh"
assert "a trailing space is refused" refused
assert "as not plain base64" contains "$ERR" "not plain base64"
assert "and no keyring is written" test ! -e "$ROOT/keyring"
variant "$ROOT/cr.sh" "AAAA$CR${NL}BBBB$CR${NL}CCCC$CR"
run "$ROOT/cr.sh"
assert "carriage returns are refused" refused
assert "as not plain base64" contains "$ERR" "not plain base64"
variant "$ROOT/tab.sh" "AAAA${NL}${TAB}BBBB${NL}CCCC"
run "$ROOT/tab.sh"
assert "a tab-indented line is refused" refused
assert "as not plain base64" contains "$ERR" "not plain base64"
variant "$ROOT/blank-spaces.sh" "AAAA${NL}  ${NL}BBBB"
run "$ROOT/blank-spaces.sh"
assert "a line of spaces inside the block is refused" refused
assert "as not plain base64" contains "$ERR" "not plain base64"

CASE='the variable of the key assigned elsewhere'
variant "$ROOT/later.sh" "$PLACEHOLDER" "$HEADER" 'PCO_RELEASE_KEY_B64=AAAA'
run "$ROOT/later.sh"
assert "a later assignment is refused, not the key of the block reported" refused
assert "as using the variable" contains "$ERR" "uses PCO_RELEASE_KEY_B64 for more than expanding it"
assert "naming the line" contains "$ERR" "line "
variant "$ROOT/append.sh" "$PLACEHOLDER" "$HEADER" 'PCO_RELEASE_KEY_B64+=AAAA'
run "$ROOT/append.sh"
assert "an append is refused" contains "$ERR" "uses PCO_RELEASE_KEY_B64 for more than expanding it"
variant "$ROOT/read2.sh" "$PLACEHOLDER" "$HEADER" "read -rd '' PCO_RELEASE_KEY_B64 <<'EOF'${NL}AAAA${NL}EOF"
run "$ROOT/read2.sh"
assert "a second read, spelled another way, is refused" contains "$ERR" "uses PCO_RELEASE_KEY_B64 for more than expanding it"
variant "$ROOT/printfv.sh" "$PLACEHOLDER" "$HEADER" "printf -v PCO_RELEASE_KEY_B64 '%s' AAAA"
run "$ROOT/printfv.sh"
assert "printf -v is refused" contains "$ERR" "uses PCO_RELEASE_KEY_B64 for more than expanding it"
variant "$ROOT/before.sh" "$PLACEHOLDER" "$HEADER" ''
{
	printf 'PCO_RELEASE_KEY_B64=old\n'
	cat "$ROOT/before.sh"
} >"$ROOT/before2.sh"
run "$ROOT/before2.sh"
assert "an assignment before the block is refused as well" contains "$ERR" "uses PCO_RELEASE_KEY_B64 for more than expanding it"
variant "$ROOT/uses.sh" "$PLACEHOLDER" "$HEADER" "# PCO_RELEASE_KEY_B64 is set above${NL}echo \"\$PCO_RELEASE_KEY_B64\" \"\${PCO_RELEASE_KEY_B64}\""
run "$ROOT/uses.sh"
assert "comments and plain expansions of it are fine" contains "$ERR" "still carries the placeholder"

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
	gpg_cmd --export "$fpr_b" >"$ROOT/b.gpg"
	gpg_cmd --export "$fpr_a" "$fpr_b" >"$ROOT/ab.gpg"

	CASE='a key with a subkey'
	variant "$ROOT/keyed.sh" "$(wrapped "$ROOT/a.gpg")"
	run "$ROOT/keyed.sh"
	assert "is accepted, wrapped as it is pasted by hand" rc_is 0
	assert "prints the fingerprint of the primary key and nothing else" equals "$OUT" "$fpr_a"
	assert "writes the keyring the installer would decode" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"
	variant "$ROOT/keyed1.sh" "$(base64 <"$ROOT/a.gpg" | tr -d '\n')"
	run "$ROOT/keyed1.sh"
	assert "is accepted on one line too" rc_is 0
	assert "with the same fingerprint" equals "$OUT" "$fpr_a"
	assert "and the same keyring" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"

	# What the installer takes: read strips the white space around the block, GNU
	# base64 skips the newlines, blank lines among them.
	CASE='white space the installer takes'
	key=$(wrapped "$ROOT/a.gpg")
	variant "$ROOT/around.sh" "${NL}  ${TAB}${key}  ${NL}${NL}"
	run "$ROOT/around.sh"
	assert "around the block is accepted" rc_is 0
	assert "with the same fingerprint" equals "$OUT" "$fpr_a"
	assert "and the same keyring" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"
	variant "$ROOT/blank.sh" "$(wrapped "$ROOT/a.gpg" | sed '2s/$/\
/')"
	run "$ROOT/blank.sh"
	assert "a blank line between two lines is accepted" rc_is 0
	assert "with the same keyring" cmp -s "$ROOT/keyring" "$ROOT/a.gpg"

	CASE='a real key damaged by white space'
	variant "$ROOT/real-space.sh" "$(wrapped "$ROOT/a.gpg" | sed '1s/$/ /')"
	run "$ROOT/real-space.sh"
	assert "a trailing space after the first line is refused" refused
	assert "as not plain base64" contains "$ERR" "not plain base64"
	variant "$ROOT/real-cr.sh" "$(wrapped "$ROOT/a.gpg" | sed "s/\$/$CR/")"
	run "$ROOT/real-cr.sh"
	assert "carriage returns are refused" refused
	assert "as not plain base64" contains "$ERR" "not plain base64"
	variant "$ROOT/real-tab.sh" "$(wrapped "$ROOT/a.gpg" | sed "2s/^/$TAB/")"
	run "$ROOT/real-tab.sh"
	assert "a tab-indented second line is refused" refused
	assert "as not plain base64" contains "$ERR" "not plain base64"
	assert "and no keyring is written" test ! -e "$ROOT/keyring"

	CASE='the home of gpg'
	mkdir "$ROOT/home"
	RC=0
	OUT=$(GNUPGHOME=$ROOT/home bash "$SUT" "$ROOT/keyed.sh" "$ROOT/keyring" 2>"$ROOT/err") || RC=$?
	ERR=$(<"$ROOT/err")
	assert "of the caller is not used" rc_is 0
	assert "and gets no files" is_empty "$(ls -A "$ROOT/home")"

	CASE='two keys'
	variant "$ROOT/two.sh" "$(wrapped "$ROOT/ab.gpg")"
	run "$ROOT/two.sh"
	assert "are accepted, for the time a key is replaced by another" rc_is 0
	assert "and both primary fingerprints are printed, one per line, in the order of the keyring" equals "$OUT" "$fpr_a${NL}$fpr_b"
	assert "and the keyring holds both" cmp -s "$ROOT/keyring" "$ROOT/ab.gpg"
}
crypto_cases

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
