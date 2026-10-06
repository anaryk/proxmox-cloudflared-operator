#!/usr/bin/env bash
#
# Tests for check-refs.sh, in a repository of its own made for the purpose: one
# file at a time, a line that cites material outside the repository must fail
# the check and a line that only looks like such a citation must pass it.
#
# Run it with: bash scripts/check-refs_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/check-refs.sh

CASE=
CHECKS=0
FAILS=0
RC=0
OUT=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-check-refs-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

repo() {
	git -C "$ROOT" "$@"
}

repo init -q

fail() {
	FAILS=$((FAILS + 1))
	printf 'FAIL [%s] %s\n' "$CASE" "$1"
}

# put <path> <content>: the repository holds that one file, staged.
put() {
	repo rm -rfq --cached . 2>/dev/null || true
	find "$ROOT" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
	mkdir -p "$ROOT/$(dirname "$1")"
	printf '%s\n' "$2" >"$ROOT/$1"
	repo add -- "$1"
}

# run [<directory>]: the check, from the root of the repository or below it.
run() {
	RC=0
	OUT=$(cd "$ROOT/${1:-.}" && bash "$SUT" 2>&1) || RC=$?
}

# cites <path> <content>: the check fails, and names the file and the line.
cites() {
	CASE=$2
	CHECKS=$((CHECKS + 1))
	put "$1" "$2"
	run
	if [[ $RC != 1 ]]; then
		fail "expected the check to fail with 1, got $RC: $OUT"
	elif [[ $OUT != *"$1:1:"* ]]; then
		fail "the output does not name $1:1: $OUT"
	fi
}

# clean <path> <content>: the check passes without a word.
clean() {
	CASE=$2
	CHECKS=$((CHECKS + 1))
	put "$1" "$2"
	run
	if [[ $RC != 0 || -n $OUT ]]; then
		fail "expected the check to pass, got $RC: $OUT"
	fi
}

# Each shape of citation the repository had.
cites internal/a.go '// the volume is mounted at start (ruling 23)'
cites internal/a.go '// Ruling 8 names the forms of a mount source'
cites internal/a.go '// never converted to wall-clock time (ruling'
cites internal/a.go '// the table of spec-ui 9.3'
cites web/a.ts '// History routing over the paths of spec-ui 3.2.'
cites internal/a.go '// as in spec section 2'
cites internal/a.go '// the spec v5 says so'
cites internal/a.go '// the limits, spec § 9.8'
cites internal/a.go '// stays foreign (gate 3, suggestion 1)'
cites internal/a.go '// Failure mode 5: a NIC configured without a link'
cites internal/a.go '// plan 1E adds the installer'
cites internal/a.go "// the values of plan 3's gateway"
cites web/a.ts '// The bands as task 11b lays them out'
cites internal/a.go '// The clone sequence of report 3 section 5'
cites internal/a.go '{name: "zfs, lab A7(e)", fixture: "zfs"},'
cites internal/a.go '// Lab 80 showed both change on reboot'
cites internal/a.go '// experiment A9 showed it'
cites internal/a.go '// the lab finding about restores'
cites internal/a.go '// (ruling 19, gate 2 recommended 2).'
cites internal/a.go '// a second take, suggestion 1'
cites internal/a.go '// Reproduced (d): a listing that succeeds without the zone.'
cites docs/a.md 'See the brief for the reason.'
cites internal/a.go '// per the brief, the cap is 400'

# Labels of one letter and digits, and where they stand.
cites internal/a.go '// kept for the caller (R19)'
cites internal/a.go '// R8 is the order of the checks'
cites internal/a.go '// G3-1 asks for it'
cites internal/a.go '// the A14 case'
cites internal/a.go '// the D2 case: a rejected target'
cites internal/a.go '// The H1 attacker holds a token'
cites internal/a.go '// stays foreign (P5)'
cites internal/a.go '// not OK without Copy when it does: recommended 1, Q2). A'
cites internal/a.go '// C1: a node whose pvestatd had died answered no uptimes.'
cites web/a.css ' * E1: a run that did not look leaves the conflicts.'
cites scripts/a.sh '# B2: a confirmation that could not be saved'
cites internal/a.go '{name: "G1: an unknown form within the uptime"},'
cites web/a.ts "test('S3: a form', () => {})"

# What only looks like a citation: the repository's own words, and data.
clean internal/a.go '// the controls of C0 and C1, which include escape'
clean internal/a.go '// Matrix row B3: the entry of the user replaces its group'"'"'s'
clean web/a.ts "'/routes/%E0%A4%A'"
clean internal/a.go '"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0,firewall=1",'
clean internal/a.go '"net1": "virtio=bc:24:11:00:92:50"'
clean test/a.md '- Kills pco.service and stops pveproxy for 35 seconds (S13).'
clean test/a.md '| S11 | A new install only observes. |'
clean internal/a.go '// a brief edit of the Notes loses nothing'
clean internal/a.go '// the brief answer of the daemon'
clean web/a.tsx '<path d="M8 1.9l6.5 11.6H1.5z" />'
clean internal/a.go '// R2 is an object storage, and not what the daemon uses'
clean internal/a.go '// the gate tag is cf-tunnel unless the settings say another'
clean internal/a.go '// the guests that carry the gate tag'
clean internal/a.go '// pco plan shows what the next cycle would do'
clean internal/a.go '// the plan is the same in every cycle'
clean internal/a.go '// the task of the reconciler'
clean docs/a.md 'Plans for a cluster: one connector for each node.'
clean docs/a.md 'A 192.0.2.10 and AAAA 2001:db8::1, two records'
clean internal/a.go '// p95 over 40 changes, at most 1.5 x the baseline'
clean internal/a.go 'const id = "a1b2c3d4"'
clean internal/a.go 'cfg := Config{Zone: "lab.example.com", Owner: "qemu/120"}'
clean internal/a.go '// a node of the lab, where the metrics were recorded'
clean internal/a.go '// Reproduced: a hold after the tunnel run showed the tunnels of that run'
clean internal/a.go '// Recommended: set it'

# Files that hold hashes or the patterns are not read.
put go.sum 'github.com/x/y v1.0.0 h1:ruling 23 spec-ui R19='
CASE='go.sum'
CHECKS=$((CHECKS + 1))
run
[[ $RC == 0 ]] || fail "go.sum was read: $OUT"

put web/package-lock.json '"integrity": "sha512-spec-ui R19 A14"'
CASE='package-lock.json'
CHECKS=$((CHECKS + 1))
run
[[ $RC == 0 ]] || fail "package-lock.json was read: $OUT"

# Every line of every file is reported, with the shape it is.
CASE='several files and shapes'
CHECKS=$((CHECKS + 1))
put internal/a.go '// (ruling 23)'
printf '%s\n' '// the table of spec-ui 9.3' >"$ROOT/b.ts"
repo add -- b.ts
run
if [[ $RC != 1 || $OUT != *"ruling: internal/a.go:1:"* || $OUT != *"spec: b.ts:1:"* ]]; then
	fail "expected both files with their shapes, got $RC: $OUT"
fi

# A staged file is read, an untracked one is not.
CASE='untracked file'
CHECKS=$((CHECKS + 1))
put internal/a.go '// fine'
printf '%s\n' '// (ruling 23)' >"$ROOT/untracked.go"
run
[[ $RC == 0 ]] || fail "an untracked file was read: $OUT"

# From a directory below the root, the whole repository is read.
CASE='below the root'
CHECKS=$((CHECKS + 1))
put cmd/pco/a.go '// (ruling 23)'
run cmd/pco
[[ $RC == 1 && $OUT == *"cmd/pco/a.go:1:"* ]] || fail "expected the file of the repository, got $RC: $OUT"

# Outside a repository there is nothing to check, and that is a failure.
CASE='outside a repository'
CHECKS=$((CHECKS + 1))
OUTSIDE=$(mktemp -d "${TMPDIR:-/tmp}/pco-check-refs-outside.XXXXXXXX") || exit 1
RC=0
OUT=$(cd "$OUTSIDE" && GIT_CEILING_DIRECTORIES=$(dirname "$OUTSIDE") bash "$SUT" 2>&1) || RC=$?
rm -rf "$OUTSIDE"
[[ $RC != 0 ]] || fail "expected a failure outside a repository"

printf '%s checks, %s failed\n' "$CHECKS" "$FAILS"
((FAILS == 0))
