#!/usr/bin/env bash
#
# Tests for install.sh. Every case runs the script with a bin directory of its
# own first on PATH: stubs for the commands that touch the system (they log
# their calls and behave as the case configures them) next to the few real
# tools the script needs. The PATH holds nothing else, so a command that a
# case leaves out is really missing.
#
# Run it with: bash scripts/install_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL=$HERE/install.sh
PLACEHOLDER=REPLACE-WITH-THE-RELEASE-KEY
KEY_HEADER="read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true"
# The keys the script carries, in the order of the block: one while there is one
# release key, two while one is replaced by the other. Changing the block means
# changing this list too.
RELEASE_FPRS=(3D326CB52862A2E91C9919EFA98A1ED57B31F91B)
PCO_BIN_LINE=PCO_BIN=/usr/bin/pco
TTY_LINE=TTY_DEVICE=/dev/tty

HASH_A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
HASH_B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
HASH_C=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
STUB_FPR=0123456789ABCDEF0123456789ABCDEF01234567
PKG=pco_1.2.3_amd64.deb
TEMPLATE=pco-appliance_1.2.3_amd64.tar.zst
JOURNAL=20261006T101500-3f2a.json
REPO=anaryk/proxmox-cloudflared-operator
RELEASE_URL=https://github.com/$REPO/releases/download/v1.2.3
QUESTION="Install on this node (host profile, the default) or as an appliance (a container, nothing on the node)? [host/appliance]"
JOURNAL_LINE=JOURNAL_DIR=/root/.pco-appliance-install

STUBBED=(id pveversion dpkg dpkg-deb curl sha256sum gpgv apt-get mktemp)
REAL=(base64 cp rm cat ls mkdir)
APT_FLAGS="install -y --no-install-recommends -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
CURL_FLAGS="curl --disable --fail --silent --show-error --location --proto =https --proto-redir =https --tlsv1.2 --connect-timeout 20 --max-time 300"
SMALL_FILE=1048576
PACKAGE_FILE=209715200

CASES=0
CHECKS=0
FAILS=0
SKIPPED=0
CASE_NAME=
CASE_DIR=
CASE_FAILED=0
CASE_ENV=()
PRINTED_ARGS=()
RUN_PREFIX=()
SCRIPT_UNDER_TEST=
RC=0
OUT=
ERR=
LOG=
GPG_HOME=

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-install-test.XXXXXXXX") || exit 1

cleanup() {
	if [[ -n $GPG_HOME ]]; then
		GNUPGHOME=$GPG_HOME gpgconf --kill all >/dev/null 2>&1
		rm -rf "$GPG_HOME"
	fi
	rm -rf "$ROOT"
}
trap cleanup EXIT

write_stub() {
	cat >"$ROOT/stub" <<'STUB'
#!/bin/sh
# One file behind every stubbed command: it logs the call, then behaves as the
# case configured it in $STUB_CASE/cfg.
name=${0##*/}
log=$STUB_CASE/calls.log
printf '%s %s\n' "$name" "$*" >>"$log"
note() { printf '  %s\n' "$*" >>"$log"; }
cfg() {
	if [ -f "$STUB_CASE/cfg/$1" ]; then cat "$STUB_CASE/cfg/$1"; else printf '%s' "$2"; fi
}
case $name in
id)
	printf '%s\n' "$(cfg uid 0)"
	;;
pveversion)
	echo "pve-manager/8.4.1/abcdef (running kernel: 6.8.12-1-pve)"
	;;
dpkg)
	printf '%s\n' "$(cfg arch amd64)"
	;;
curl)
	out=
	url=
	while [ $# -gt 0 ]; do
		case $1 in
		--output) out=$2; shift ;;
		http://* | https://*) url=$1 ;;
		esac
		shift
	done
	fail_on=$(cfg curl-fail-on __never__)
	case $url in
	*"$fail_on")
		echo "curl: (22) The requested URL returned error: 404" >&2
		exit 22
		;;
	esac
	file=${url##*/}
	if [ "$file" = latest ]; then src=$STUB_CASE/cfg/api-response; else src=$STUB_CASE/fixtures/$file; fi
	if [ ! -f "$src" ]; then
		echo "curl: (22) The requested URL returned error: 404" >&2
		exit 22
	fi
	if [ -n "$out" ]; then cat "$src" >"$out"; else cat "$src"; fi
	;;
sha256sum)
	for last in "$@"; do :; done
	printf '%s  %s\n' "$(cfg "sha256-${last##*/}" "$(cfg sha256 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)")" "$last"
	;;
dpkg-deb)
	# dpkg-deb -x PACKAGE DIRECTORY: the package holds the stub as usr/bin/pco.
	for last in "$@"; do :; done
	if [ -f "$STUB_CASE/cfg/dpkg-deb-fail" ]; then
		echo "dpkg-deb: error: not a Debian format archive" >&2
		exit 2
	fi
	note "dpkg-deb: package file exists: $([ -f "$2" ] && echo yes || echo no)"
	mkdir "$last" || exit 1
	if [ ! -f "$STUB_CASE/cfg/dpkg-deb-no-pco" ]; then
		mkdir -p "$last/usr/bin"
		cp "$0" "$last/usr/bin/pco"
	fi
	;;
gpgv)
	keyring=
	while [ $# -gt 0 ]; do
		case $1 in
		--keyring) keyring=$2; shift ;;
		esac
		shift
	done
	note "gpgv: keyring holds: $(cat "$keyring")"
	mode=$(cfg gpgv good)
	case $mode in
	good | extra-*)
		echo "[GNUPG:] NEWSIG"
		echo "[GNUPG:] GOODSIG 4403134754835ADB Test"
		echo "[GNUPG:] VALIDSIG 0123456789ABCDEF0123456789ABCDEF01234567 2026-01-01 1767225600 0 4 0 22 10 00 0123456789ABCDEF0123456789ABCDEF01234567"
		case $mode in
		extra-*) echo "[GNUPG:] ${mode#extra-} 4403134754835ADB Test" ;;
		esac
		;;
	no-validsig)
		echo "[GNUPG:] GOODSIG 4403134754835ADB Test"
		;;
	no-goodsig)
		echo "[GNUPG:] NEWSIG"
		echo "[GNUPG:] VALIDSIG 0123456789ABCDEF0123456789ABCDEF01234567 2026-01-01 1767225600 0 4 0 22 10 00 0123456789ABCDEF0123456789ABCDEF01234567"
		;;
	revoked | expired)
		# What gpgv prints for such a key: no GOODSIG, and it still exits 0.
		echo "[GNUPG:] NEWSIG"
		if [ "$mode" = revoked ]; then echo "[GNUPG:] REVKEYSIG 4403134754835ADB Test"; else echo "[GNUPG:] EXPKEYSIG 4403134754835ADB Test"; fi
		echo "[GNUPG:] VALIDSIG 0123456789ABCDEF0123456789ABCDEF01234567 2026-01-01 1767225600 0 4 0 22 10 00 0123456789ABCDEF0123456789ABCDEF01234567"
		;;
	*)
		echo "gpgv: BAD signature from Test" >&2
		exit 1
		;;
	esac
	;;
sqv)
	keyring=
	while [ $# -gt 0 ]; do
		case $1 in
		--keyring) keyring=$2; shift ;;
		esac
		shift
	done
	note "sqv: keyring holds: $(cat "$keyring")"
	case $(cfg sqv good) in
	good)
		echo "0123456789ABCDEF0123456789ABCDEF01234567"
		;;
	no-fingerprint)
		echo "Verified signature"
		;;
	*)
		echo "Missing key 0123456789ABCDEF0123456789ABCDEF01234567, which is needed to verify signature." >&2
		exit 1
		;;
	esac
	;;
mktemp)
	if [ -f "$STUB_CASE/cfg/mktemp-fail" ]; then
		echo "mktemp: failed to create directory" >&2
		exit 1
	fi
	for template in "$@"; do :; done
	prefix=${template##*/}
	prefix=${prefix%%X*}
	dir=$STUB_CASE/tmp/${prefix}t$$
	mkdir "$dir" || exit 1
	echo "$dir"
	;;
apt-get)
	for last in "$@"; do :; done
	note "apt-get: DEBIAN_FRONTEND=${DEBIAN_FRONTEND:-unset}"
	if [ -f "$last" ]; then note "apt-get: package file exists"; fi
	set -- $(ls -ld "${last%/*}")
	note "apt-get: directory mode $1"
	exit "$(cfg apt-rc 0)"
	;;
pco)
	n=0
	for f in "$STUB_CASE"/tmp/*; do
		if [ -e "$f" ]; then n=$((n + 1)); fi
	done
	note "pco: run as $0"
	note "pco: temporary directories left: $n"
	if [ -t 0 ]; then note "pco: stdin is a terminal"; else note "pco: stdin is not a terminal"; fi
	if [ -f "$STUB_CASE/cfg/pco-read-stdin" ]; then note "pco: stdin holds: $(cat)"; fi
	# What an installer that dies leaves behind, and a kill of the script
	# that runs it.
	if [ -f "$STUB_CASE/cfg/pco-journal" ]; then
		mkdir -p "$STUB_CASE/journals"
		echo '{}' >"$STUB_CASE/journals/$(cfg pco-journal)"
	fi
	if [ -f "$STUB_CASE/cfg/pco-signal-parent" ]; then kill "-$(cfg pco-signal-parent)" "$PPID"; fi
	exit "$(cfg pco-rc 0)"
	;;
esac
STUB
	chmod +x "$ROOT/stub"
}

# Runs a command with a pseudo terminal as its stdin, stdout and stderr, types
# PTY_INPUT at it when that is set, and passes on its output and exit code.
# pty.spawn is not used: it hangs on the Python 3.9 that ships with macOS when
# its own stdin is closed.
write_pty_runner() {
	cat >"$ROOT/pty_run.py" <<'PYTHON'
import os, pty, select, signal, sys, time

pid, fd = pty.fork()
if pid == 0:
    os.execvp(sys.argv[1], sys.argv[1:])

# What is typed at the terminal, taken from PTY_INPUT.
typed = os.environ.get("PTY_INPUT")
if typed:
    os.write(fd, typed.encode())

deadline = time.time() + 60
out = b""
status = None
while time.time() < deadline:
    ready, _, _ = select.select([fd], [], [], 0.2)
    if ready:
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        out += data
    else:
        done, st = os.waitpid(pid, os.WNOHANG)
        if done:
            status = st
            break
if status is None:
    if time.time() >= deadline:
        os.kill(pid, signal.SIGKILL)
    _, status = os.waitpid(pid, 0)
sys.stdout.buffer.write(out)
sys.exit(os.WEXITSTATUS(status) if os.WIFEXITED(status) else 1)
PYTHON
}

# A copy of install.sh with the key block, whatever it holds, swapped for a
# key, and the lines that name the installed binary, the terminal and the
# directory of the journals swapped for other paths: a test can put nothing at
# /usr/bin/pco, has no terminal and must not write to /root. The script takes
# none of them from the environment, so that root's cannot change them. Nothing
# else differs from the real script.
make_variant_script() {
	local out=$1 key=$2 pco_bin=$3 tty=$4 journals=${5:-$ROOT/no-journals}
	local line keys=0 bins=0 ttys=0 dirs=0 inside=0
	while IFS= read -r line; do
		if [[ $inside == 1 ]]; then
			# The lines of the block are dropped; the EOF that closes it stays.
			if [[ $line == EOF ]]; then
				inside=0
				printf '%s\n' "$line"
			fi
		elif [[ $line == "$KEY_HEADER" ]]; then
			printf '%s\n%s\n' "$line" "$key"
			inside=1
			keys=$((keys + 1))
		elif [[ $line == "$PCO_BIN_LINE" ]]; then
			printf 'PCO_BIN=%s\n' "$pco_bin"
			bins=$((bins + 1))
		elif [[ $line == "$TTY_LINE" ]]; then
			printf 'TTY_DEVICE=%s\n' "$tty"
			ttys=$((ttys + 1))
		elif [[ $line == "$JOURNAL_LINE" ]]; then
			printf 'JOURNAL_DIR=%s\n' "$journals"
			dirs=$((dirs + 1))
		else
			printf '%s\n' "$line"
		fi
	done <"$INSTALL" >"$out"
	if [[ $keys != 1 || $inside != 0 || $bins != 1 || $ttys != 1 || $dirs != 1 ]]; then
		echo "install_test.sh: install.sh has $keys closed key blocks, $bins lines naming the binary, $ttys naming the terminal and $dirs naming the journals, expected one of each" >&2
		exit 1
	fi
}

# The key block as a case sees it: two lines, as a key pasted by hand usually is.
fake_key() {
	local b64
	b64=$(printf 'fake-keyring-bytes' | base64 | tr -d '\n')
	printf '%s\n%s' "${b64:0:8}" "${b64:8}"
}

new_case() {
	local t
	CASE_NAME=$1
	CASE_FAILED=0
	CASES=$((CASES + 1))
	CASE_DIR=$ROOT/case$CASES
	mkdir -p "$CASE_DIR/bin" "$CASE_DIR/home" "$CASE_DIR/tmp" "$CASE_DIR/untrusted" "$CASE_DIR/cfg" \
		"$CASE_DIR/fixtures" "$CASE_DIR/work" "$CASE_DIR/local"
	for t in "${STUBBED[@]}"; do
		ln -s "$ROOT/stub" "$CASE_DIR/bin/$t"
	done
	for t in "${REAL[@]}"; do
		ln -s "$(command -v "$t")" "$CASE_DIR/bin/$t"
	done
	CASE_ENV=()
	RUN_PREFIX=()
	SCRIPT_UNDER_TEST=$KEYED
	printf 'deb-bytes' >"$CASE_DIR/fixtures/$PKG"
	printf '%s  %s\n%s  pco_1.2.3_arm64.deb\n' "$HASH_A" "$PKG" "$HASH_B" >"$CASE_DIR/fixtures/checksums.txt"
	printf 'sig-bytes' >"$CASE_DIR/fixtures/checksums.txt.sig"
	printf '{\n  "url": "https://api.github.com/x",\n  "tag_name": "v1.2.3",\n  "name": "1.2.3"\n}\n' \
		>"$CASE_DIR/cfg/api-response"
}

set_cfg() {
	printf '%s' "$2" >"$CASE_DIR/cfg/$1"
}

set_env() {
	CASE_ENV+=("$1=$2")
}

drop_command() {
	rm -f "$CASE_DIR/bin/$1"
}

add_stub() {
	ln -s "$ROOT/stub" "$CASE_DIR/bin/$1"
}

# The script under test, with the binary it hands over to, the terminal it
# reads from and the directory it looks for journals in at other paths than the
# usual; the journals of a case are in a directory of its own.
use_variant() {
	local pco_bin=$1 tty=$2
	make_variant_script "$CASE_DIR/install.sh" "$(fake_key)" "$pco_bin" "$tty" "$CASE_DIR/journals"
	SCRIPT_UNDER_TEST=$CASE_DIR/install.sh
}

# A case that has a terminal, which holds TEXT for the script to read.
use_terminal() {
	printf '%s' "$1" >"$CASE_DIR/tty"
	use_variant "$ROOT/usrbin/pco" "$CASE_DIR/tty"
}

# A journal in the directory the script looks in, as an earlier run left it.
leave_journal() {
	mkdir -p "$CASE_DIR/journals"
	printf '{}\n' >"$CASE_DIR/journals/$1"
}

use_real() {
	local t
	for t in "$@"; do
		rm -f "$CASE_DIR/bin/$t"
		ln -s "$(command -v "$t")" "$CASE_DIR/bin/$t"
	done
}

# The files of the offline mode, copied from the case's fixtures.
use_local_files() {
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/local/$PKG"
	cp "$CASE_DIR/fixtures/checksums.txt" "$CASE_DIR/local/checksums.txt"
	cp "$CASE_DIR/fixtures/checksums.txt.sig" "$CASE_DIR/local/checksums.txt.sig"
	set_env PCO_DEB "$CASE_DIR/local/$PKG"
	set_env PCO_CHECKSUMS "$CASE_DIR/local/checksums.txt"
	set_env PCO_SIGNATURE "$CASE_DIR/local/checksums.txt.sig"
}

run_install() {
	: >"$CASE_DIR/calls.log"
	(
		cd "$CASE_DIR/work" || exit 99
		${RUN_PREFIX[@]+"${RUN_PREFIX[@]}"} env -i \
			"PATH=$CASE_DIR/bin" "HOME=$CASE_DIR/home" "TMPDIR=$CASE_DIR/untrusted" \
			"STUB_CASE=$CASE_DIR" \
			${CASE_ENV[@]+"${CASE_ENV[@]}"} \
			"$BASH" "$SCRIPT_UNDER_TEST" "$@"
	) >"$CASE_DIR/out" 2>"$CASE_DIR/err" </dev/null
	RC=$?
	OUT=$(<"$CASE_DIR/out")
	ERR=$(<"$CASE_DIR/err")
	LOG=$(<"$CASE_DIR/calls.log")
}

fail() {
	FAILS=$((FAILS + 1))
	printf 'FAIL [%s] %s\n' "$CASE_NAME" "$1"
	if [[ $CASE_FAILED == 0 ]]; then
		CASE_FAILED=1
		printf '  exit code: %s\n  stdout:\n%s\n  stderr:\n%s\n  calls:\n%s\n' \
			"$RC" "${OUT//$'\n'/$'\n    '}" "${ERR//$'\n'/$'\n    '}" "${LOG//$'\n'/$'\n    '}" | sed 's/^/  /'
	fi
}

# ensure DESCRIPTION COMMAND...: counts a check, which fails when COMMAND does.
ensure() {
	local description=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		fail "$description"
	fi
}

equals() {
	[[ $1 == "$2" ]]
}

differs() {
	[[ $1 != "$2" ]]
}

contains() {
	[[ $1 == *"$2"* ]]
}

omits() {
	[[ $1 != *"$2"* ]]
}

starts_with() {
	[[ $1 == "$2"* ]]
}

# matches TEXT PATTERN: PATTERN is a glob that has to fit the whole text.
matches() {
	# shellcheck disable=SC2254 # the pattern is a glob on purpose
	case $1 in
	$2) return 0 ;;
	esac
	return 1
}

is_file() {
	[[ -f $1 ]]
}

is_absent() {
	[[ ! -e $1 ]]
}

dir_is_empty() {
	local f
	for f in "$1"/* "$1"/.[!.]*; do
		if [[ -e $f ]]; then
			return 1
		fi
	done
	return 0
}

count_lines() {
	local newlines
	if [[ -z $1 ]]; then
		echo 0
		return
	fi
	newlines=${1//[!$'\n']/}
	echo $((${#newlines} + 1))
}

assert_rc() {
	ensure "exit code is $1, got $RC" equals "$RC" "$1"
}

# The failure contract: non-zero exit, one line on stderr, starting with the
# script's name, saying what is wrong.
assert_fail() {
	local lines
	lines=$(count_lines "$ERR")
	ensure "the script exits non-zero" differs "$RC" 0
	ensure "stderr is one line, got $lines" equals "$lines" 1
	ensure "stderr starts with 'install.sh: '" starts_with "$ERR" "install.sh: "
	ensure "stderr mentions '$1'" contains "$ERR" "$1"
}

assert_stdout_has() {
	ensure "stdout has '$1'" contains "$OUT" "$1"
}

assert_stdout_lacks() {
	ensure "stdout lacks '$1'" omits "$OUT" "$1"
}

assert_stderr_has() {
	ensure "stderr has '$1'" contains "$ERR" "$1"
}

assert_stderr_lacks() {
	ensure "stderr lacks '$1'" omits "$ERR" "$1"
}

assert_stderr_empty() {
	ensure "stderr is empty, got '$ERR'" equals "$ERR" ""
}

# $1 is a glob pattern.
assert_log_has() {
	ensure "the call log has '$1'" matches "$LOG" "*$1*"
}

assert_log_lacks() {
	ensure "the call log lacks '$1'" omits_glob "$LOG" "$1"
}

omits_glob() {
	! matches "$1" "*$2*"
}

# $1 is the whole text of a line of the call log.
assert_log_line() {
	ensure "the call log has the line '$1'" has_line "$LOG" "$1"
}

has_line() {
	local line
	while IFS= read -r line; do
		if [[ $line == "$2" ]]; then
			return 0
		fi
	done <<<"$1"
	return 1
}

# Calls start at the beginning of a line; what the stubs note is indented.
calls() {
	local n=0 line
	while IFS= read -r line; do
		if [[ $line == "$1 "* ]]; then
			n=$((n + 1))
		fi
	done <<<"$LOG"
	echo "$n"
}

assert_calls() {
	local n
	n=$(calls "$1")
	ensure "$1 was called $2 times, got $n" equals "$n" "$2"
}

line_of() {
	local n=0 line
	while IFS= read -r line; do
		n=$((n + 1))
		if [[ $line == "$1 "* ]]; then
			echo "$n"
			return
		fi
	done <<<"$LOG"
	echo 0
}

# The URLs the script asked curl for, in order, separated by spaces.
curl_urls() {
	local line urls=
	while IFS= read -r line; do
		if [[ $line == "curl "* ]]; then
			urls="$urls ${line##* }"
		fi
	done <<<"$LOG"
	echo "${urls# }"
}

is_before() {
	[[ $1 != 0 && $2 != 0 && $1 -lt $2 ]]
}

assert_before() {
	ensure "$1 comes before $2 in the call log" is_before "$(line_of "$1")" "$(line_of "$2")"
}

assert_tmp_gone() {
	ensure "the temporary directory is gone" dir_is_empty "$CASE_DIR/tmp"
}

assert_nothing_elsewhere() {
	ensure "nothing was written to the working directory" dir_is_empty "$CASE_DIR/work"
	ensure "nothing was written to the home directory" dir_is_empty "$CASE_DIR/home"
	ensure "nothing was written to the directory TMPDIR names" dir_is_empty "$CASE_DIR/untrusted"
}

assert_nothing_installed() {
	assert_calls apt-get 0
	assert_calls pco 0
}

assert_nothing_unpacked() {
	assert_nothing_installed
	assert_calls dpkg-deb 0
}

# Sets the case up to run on a pseudo terminal that INPUT is typed at. It
# counts a skip and returns 1 when there is no python3 to make one.
use_pty() {
	if ! command -v python3 >/dev/null 2>&1; then
		echo "skipped [$CASE_NAME]: no python3 to open a pseudo terminal"
		SKIPPED=$((SKIPPED + 1))
		return 1
	fi
	write_pty_runner
	RUN_PREFIX=(env "PTY_INPUT=$1" python3 "$ROOT/pty_run.py")
}

# The template of the appliance as a local file, with the line of checksums.txt
# that names it, and PCO_TEMPLATE pointing at it.
use_template() {
	printf 'template-bytes' >"$CASE_DIR/local/$TEMPLATE"
	printf '%s  %s\n%s  %s\n' "$HASH_A" "$PKG" "$HASH_C" "$TEMPLATE" >"$CASE_DIR/fixtures/checksums.txt"
	set_cfg "sha256-$TEMPLATE" "$HASH_C"
	set_env PCO_TEMPLATE "$CASE_DIR/local/$TEMPLATE"
}

# The command a message of the script gives, after MARKER on a line of TEXT, as
# ENV... install.sh ARGS...
printed_after() {
	local line
	while IFS= read -r line; do
		if [[ $line == *"$2"* ]]; then
			printf '%s\n' "${line#*"$2"}"
			return 0
		fi
	done <<<"$1"
	return 1
}

# Sets the next run up as the command says: its VAR=value words become the
# environment, and the words after install.sh the arguments, which are left in
# PRINTED_ARGS.
use_printed() {
	local word seen=0
	local -a words
	# The line is quoted the way a shell reads it.
	eval "words=($1)"
	CASE_ENV=()
	PRINTED_ARGS=()
	for word in "${words[@]}"; do
		if [[ $seen == 1 ]]; then
			PRINTED_ARGS+=("$word")
		elif [[ $word == install.sh ]]; then
			seen=1
		else
			set_env "${word%%=*}" "${word#*=}"
		fi
	done
	ensure "the command names install.sh" equals "$seen" 1
}

case_not_root() {
	new_case "a user other than root stops before any download"
	set_cfg uid 1000
	run_install
	assert_fail "run the script as root"
	assert_calls curl 0
	assert_calls pveversion 0
	assert_nothing_installed
	assert_tmp_gone
}

case_no_pveversion() {
	new_case "a node without pveversion stops"
	drop_command pveversion
	run_install
	assert_fail "pveversion not found"
	assert_calls curl 0
	assert_nothing_installed
}

case_arch() {
	local arch
	for arch in armhf i386 riscv64; do
		new_case "architecture $arch is refused"
		set_cfg arch "$arch"
		run_install
		assert_fail "unsupported architecture $arch"
		assert_calls curl 0
		assert_nothing_installed
	done
	new_case "architecture arm64 downloads the arm64 package"
	set_cfg arch arm64
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/fixtures/pco_1.2.3_arm64.deb"
	set_cfg sha256 "$HASH_B"
	run_install
	assert_rc 0
	assert_log_has "$RELEASE_URL/pco_1.2.3_arm64.deb"
	assert_stdout_has "architecture arm64"
}

case_preflight_order() {
	new_case "root is checked before pveversion"
	set_cfg uid 1000
	drop_command pveversion
	run_install
	assert_fail "run the script as root"

	new_case "pveversion is checked before the architecture"
	set_cfg arch armhf
	drop_command pveversion
	run_install
	assert_fail "pveversion not found"

	new_case "the architecture is checked before the required commands"
	set_cfg arch armhf
	drop_command curl
	run_install
	assert_fail "unsupported architecture"
}

case_required_commands() {
	local cmd
	for cmd in curl sha256sum base64 apt-get mktemp; do
		new_case "a missing $cmd stops before any download"
		drop_command "$cmd"
		run_install
		assert_fail "missing required command(s): $cmd"
		assert_calls curl 0
		assert_nothing_installed
	done
	new_case "every missing command is named"
	drop_command curl
	drop_command base64
	run_install
	assert_fail "missing required command(s): curl, base64"

	new_case "without gpgv and sqv it says how to get one"
	drop_command gpgv
	run_install
	assert_fail "missing required command(s): gpgv or sqv (for the signature check run: apt-get install gpgv)"
	assert_calls curl 0
	assert_nothing_installed

	new_case "the hint about gpgv comes after the other missing commands"
	drop_command gpgv
	drop_command curl
	run_install
	assert_fail "missing required command(s): curl, gpgv or sqv (for the signature check run: apt-get install gpgv)"

	new_case "the offline mode does not need curl"
	drop_command curl
	use_local_files
	run_install
	assert_rc 0
	assert_calls apt-get 1
}

case_verifier_choice() {
	new_case "gpgv alone checks the signature"
	run_install
	assert_rc 0
	assert_calls gpgv 1
	assert_calls sqv 0
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"

	new_case "sqv alone checks the signature"
	drop_command gpgv
	add_stub sqv
	run_install
	assert_rc 0
	assert_calls sqv 1
	assert_calls gpgv 0
	assert_log_has "sqv --keyring $CASE_DIR/tmp/pco-install.*/release.gpg $CASE_DIR/tmp/pco-install.*/checksums.txt.sig $CASE_DIR/tmp/pco-install.*/checksums.txt"
	assert_log_has "sqv: keyring holds: fake-keyring-bytes"
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	assert_before sqv sha256sum
	assert_before sha256sum apt-get

	new_case "with both, sqv is preferred and gpgv is not run"
	add_stub sqv
	run_install
	assert_rc 0
	assert_calls sqv 1
	assert_calls gpgv 0
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"

	new_case "sqv that rejects the signature stops before the checksum is looked at"
	drop_command gpgv
	add_stub sqv
	set_cfg sqv bad
	printf '%s  pco_1.2.3_arm64.deb\n' "$HASH_B" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "signature of checksums.txt is not valid"
	assert_calls sha256sum 0
	assert_nothing_installed
	assert_tmp_gone

	new_case "sqv that accepts without naming a key stops"
	drop_command gpgv
	add_stub sqv
	set_cfg sqv no-fingerprint
	run_install
	assert_fail "named no key fingerprint"
	assert_nothing_installed

	new_case "the offline mode uses sqv too"
	drop_command gpgv
	add_stub sqv
	use_local_files
	run_install
	assert_rc 0
	assert_calls sqv 1
	assert_calls curl 0
}

case_placeholder_key() {
	new_case "the placeholder key refuses to install"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	run_install
	assert_fail "this build of the script carries no release key, so it cannot check the signature; PCO_INSECURE_SKIP_SIGNATURE=1 installs without that check, and then only the HTTPS connection to the release host decides what is installed"
	assert_calls curl 0
	assert_nothing_installed
	assert_tmp_gone

	new_case "the placeholder key refuses an offline install too"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	use_local_files
	run_install
	assert_fail "carries no release key"
	assert_stderr_has "and then only whoever supplied the files decides what is installed"
	assert_nothing_installed

	new_case "the placeholder key goes on with the override, and says so"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	run_install
	assert_rc 0
	assert_stderr_has "install.sh: WARNING: PCO_INSECURE_SKIP_SIGNATURE=1, the signature of checksums.txt is NOT checked"
	assert_stderr_has "install.sh: WARNING: only the HTTPS connection to the release host decides what is installed; the checksum only catches a damaged download"
	assert_stdout_has "verified by: checksum only (signature check skipped)"
	assert_stdout_lacks "signature by key"
	assert_calls gpgv 0
	assert_log_lacks "checksums.txt.sig"
	assert_calls apt-get 1

	new_case "offline with the override, it says that the files decide"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	use_local_files
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	run_install
	assert_rc 0
	assert_stderr_has "install.sh: WARNING: only whoever supplied the files decides what is installed; the checksum only catches a damaged file"
	assert_stderr_lacks "HTTPS"
	assert_calls curl 0

	new_case "the override does not skip the checksum"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	set_cfg sha256 "$HASH_B"
	run_install
	assert_stderr_has "does not match"
	assert_nothing_installed

	new_case "the override only counts as 1"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	set_env PCO_INSECURE_SKIP_SIGNATURE yes
	run_install
	assert_fail "carries no release key"
}

case_override_with_a_key() {
	new_case "the override skips the signature even when the script has a key"
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	run_install
	assert_rc 0
	assert_calls gpgv 0
	assert_stdout_has "checksum only (signature check skipped)"
}

case_bad_key_block() {
	new_case "a key block that is not base64 stops"
	make_variant_script "$CASE_DIR/install.sh" '!!! not base64 !!!' "$ROOT/usrbin/pco" "$ROOT/no-tty"
	SCRIPT_UNDER_TEST=$CASE_DIR/install.sh
	run_install
	assert_fail "not valid base64"
	assert_calls gpgv 0
	assert_nothing_installed
}

case_bad_signature() {
	new_case "a bad signature stops before the checksum is looked at and before apt-get"
	set_cfg gpgv bad
	# No line for the package: if the checksum were read, the error would say so.
	printf '%s  pco_1.2.3_arm64.deb\n' "$HASH_B" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "signature of checksums.txt is not valid"
	assert_calls gpgv 1
	assert_calls sha256sum 0
	assert_nothing_installed
	assert_tmp_gone
	assert_nothing_elsewhere

	new_case "gpgv that names no key fingerprint stops"
	set_cfg gpgv no-validsig
	run_install
	assert_fail "named no key fingerprint"
	assert_nothing_installed

	new_case "gpgv that does not call the signature good stops"
	set_cfg gpgv no-goodsig
	run_install
	assert_fail "signature of checksums.txt is not valid for the release key"
	assert_nothing_installed
	assert_tmp_gone
}

case_gpgv_statuses() {
	local status
	new_case "a signature by a revoked key is refused although gpgv exits 0"
	set_cfg gpgv revoked
	run_install
	assert_fail "signature of checksums.txt is not valid for the release key, or the key was revoked or has expired"
	assert_calls sha256sum 0
	assert_nothing_installed
	assert_tmp_gone

	new_case "a signature by an expired key is refused although gpgv exits 0"
	set_cfg gpgv expired
	run_install
	assert_fail "or the key was revoked or has expired"
	assert_calls sha256sum 0
	assert_nothing_installed

	for status in BADSIG ERRSIG EXPSIG EXPKEYSIG REVKEYSIG; do
		new_case "a $status line next to a good one is refused"
		set_cfg gpgv "extra-$status"
		run_install
		assert_fail "signature of checksums.txt is not valid for the release key"
		assert_calls sha256sum 0
		assert_nothing_installed
	done
}

case_signature_call() {
	new_case "gpgv gets the decoded keyring, the signature and the checksums"
	run_install
	assert_rc 0
	assert_log_has "gpgv --status-fd 1 --keyring $CASE_DIR/tmp/pco-install.*/release.gpg $CASE_DIR/tmp/pco-install.*/checksums.txt.sig $CASE_DIR/tmp/pco-install.*/checksums.txt"
	assert_log_has "gpgv: keyring holds: fake-keyring-bytes"
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	assert_before gpgv sha256sum
	assert_before sha256sum apt-get
}

case_checksum() {
	new_case "a checksum that does not match stops before apt-get"
	set_cfg sha256 "$HASH_B"
	run_install
	assert_fail "checksum of $PKG does not match checksums.txt"
	assert_stderr_has "expected $HASH_A, got $HASH_B"
	assert_nothing_installed
	assert_tmp_gone
	assert_nothing_elsewhere

	new_case "a checksums file without a line for the package stops"
	printf '%s  pco_1.2.3_arm64.deb\n' "$HASH_B" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "checksums.txt has no line for $PKG"
	assert_calls sha256sum 0
	assert_nothing_installed

	new_case "two lines for the package stop"
	printf '%s  %s\n%s  %s\n' "$HASH_A" "$PKG" "$HASH_A" "$PKG" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "checksums.txt has 2 lines for $PKG"
	assert_nothing_installed

	new_case "two lines that disagree stop too"
	printf '%s  %s\n%s  %s\n' "$HASH_B" "$PKG" "$HASH_A" "$PKG" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "checksums.txt has 2 lines for $PKG"
	assert_nothing_installed

	new_case "a file name that only contains the package name is not the package"
	printf '%s  ./%s\n%s  dist/%s\n%s  %s.sig\n%s  x%s\n%s %s\n' \
		"$HASH_A" "$PKG" "$HASH_A" "$PKG" "$HASH_A" "$PKG" "$HASH_A" "$PKG" "$HASH_A" "$PKG" \
		>"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_fail "checksums.txt has no line for $PKG"
	assert_nothing_installed

	new_case "the last line may lack its newline"
	printf '%s  pco_1.2.3_arm64.deb\n%s  %s' "$HASH_B" "$HASH_A" "$PKG" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_rc 0
	assert_calls apt-get 1

	new_case "checksums.txt is never handed to sha256sum"
	run_install
	assert_rc 0
	assert_calls sha256sum 1
	assert_log_has "sha256sum $CASE_DIR/tmp/pco-install.*/$PKG"
	assert_log_lacks "sha256sum -c"
	assert_log_lacks "sha256sum $CASE_DIR/tmp/pco-install.*/checksums.txt"
}

case_latest_version() {
	new_case "the latest release comes from the GitHub API"
	run_install
	assert_rc 0
	ensure "the API is asked first, then the package, the checksums and the signature are downloaded: $(curl_urls)" \
		equals "$(curl_urls)" "https://api.github.com/repos/$REPO/releases/latest $RELEASE_URL/$PKG $RELEASE_URL/checksums.txt $RELEASE_URL/checksums.txt.sig"
	assert_before curl apt-get
	assert_stdout_has "package: pco 1.2.3, architecture amd64"
	assert_stdout_has "release: $RELEASE_URL/"

	new_case "a one line JSON answer of the API is read too"
	printf '{"url":"x","tag_name":"v1.2.3","name":"1.2.3","draft":false}' >"$CASE_DIR/cfg/api-response"
	run_install
	assert_rc 0
	assert_log_has "$RELEASE_URL/$PKG"

	new_case "an answer without a tag stops"
	printf '{"message": "API rate limit exceeded"}\n' >"$CASE_DIR/cfg/api-response"
	run_install
	assert_fail "names no release"
	assert_calls curl 1
	assert_nothing_installed
	assert_tmp_gone

	new_case "a failing API request stops"
	set_cfg curl-fail-on latest
	run_install
	assert_fail "download failed: https://api.github.com/repos/$REPO/releases/latest (curl: (22)"
	assert_nothing_installed
}

case_pinned_version() {
	new_case "PCO_VERSION pins the download URL and skips the API"
	set_env PCO_VERSION 1.2.3
	run_install
	assert_rc 0
	assert_log_lacks "api.github.com"
	assert_log_has "$RELEASE_URL/$PKG"
	assert_calls curl 3

	new_case "PCO_VERSION may carry a leading v"
	set_env PCO_VERSION v1.2.3
	run_install
	assert_rc 0
	assert_log_has "$RELEASE_URL/$PKG"
	assert_log_lacks "vv1.2.3"

	new_case "PCO_VERSION pins another release than the latest"
	set_env PCO_VERSION 0.9.1
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/fixtures/pco_0.9.1_amd64.deb"
	printf '%s  pco_0.9.1_amd64.deb\n' "$HASH_A" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_rc 0
	assert_log_has "https://github.com/$REPO/releases/download/v0.9.1/pco_0.9.1_amd64.deb"
	assert_log_lacks "1.2.3"

	new_case "a pre-release version is accepted"
	set_env PCO_VERSION 1.3.0-rc.1
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/fixtures/pco_1.3.0-rc.1_amd64.deb"
	printf '%s  pco_1.3.0-rc.1_amd64.deb\n' "$HASH_A" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_rc 0
	assert_log_has "v1.3.0-rc.1/pco_1.3.0-rc.1_amd64.deb"
}

case_refused_versions() {
	local bad
	# shellcheck disable=SC2016 # the values are hostile on purpose and must not expand
	for bad in 'v1.2.3;touch pwned' 'v1.2.3$(touch pwned)' 'v1.2.3`touch pwned`' 'v1.2.3/../x' \
		'1.2.3/../../x' 'v1/2/3' '../1.2.3' 'v1.2.3 x' 'v1.2.3|x' 'v1.2.3&x' 'v1.2' 'latest' \
		'v1.2.3-' 'v1.2.3-a/b' "v1.2.3'" 'v1.2.3\x'; do
		new_case "the version '$bad' from the API is refused"
		printf '{\n  "tag_name": "%s"\n}\n' "$bad" >"$CASE_DIR/cfg/api-response"
		run_install
		assert_fail "refusing the version"
		assert_calls curl 1
		assert_nothing_installed
		assert_tmp_gone
		ensure "the metacharacters were not run" is_absent "$CASE_DIR/work/pwned"
	done
	new_case "an empty PCO_VERSION counts as not set"
	set_env PCO_VERSION ""
	run_install
	assert_rc 0
	assert_log_has "api.github.com"
	# shellcheck disable=SC2016 # the values are hostile on purpose and must not expand
	for bad in 'v1.2.3;touch pwned' '$(touch pwned)' 'v1.2.3/../x' '1.2' 'v1.2.3'$'\n''x' '1.2.3'$'\n'; do
		new_case "PCO_VERSION '${bad//$'\n'/\\n}' is refused"
		set_env PCO_VERSION "$bad"
		run_install
		assert_fail "refusing the version"
		assert_calls curl 0
		assert_nothing_installed
		ensure "the metacharacters were not run" is_absent "$CASE_DIR/work/pwned"
	done
}

case_version_length() {
	local v64 v65 zeros
	zeros=$(printf '%058d' 0)
	v64=1.2.3-$zeros
	v65=1.2.3-${zeros}0

	new_case "a version of 64 characters is accepted"
	set_env PCO_VERSION "$v64"
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/fixtures/pco_${v64}_amd64.deb"
	printf '%s  pco_%s_amd64.deb\n' "$HASH_A" "$v64" >"$CASE_DIR/fixtures/checksums.txt"
	run_install
	assert_rc 0
	assert_log_has "v$v64/pco_${v64}_amd64.deb"

	new_case "a version of 65 characters is refused"
	set_env PCO_VERSION "$v65"
	run_install
	assert_fail "refusing the version"
	ensure "the error does not repeat the whole version" omits "$ERR" "$v65"
	assert_calls curl 0
	assert_nothing_installed

	new_case "a leading v counts towards the 64 characters"
	set_env PCO_VERSION "v$v64"
	run_install
	assert_fail "refusing the version"
	assert_calls curl 0

	new_case "a version of 65 characters from the API is refused"
	printf '{\n  "tag_name": "%s"\n}\n' "$v65" >"$CASE_DIR/cfg/api-response"
	run_install
	assert_fail "refusing the version"
	assert_calls curl 1
	assert_nothing_installed
}

case_repo() {
	local bad
	# shellcheck disable=SC2016 # the values are hostile on purpose and must not expand
	for bad in evil a/b/c 'a b/c' 'a/b;c' '../x' 'x/..' './x' 'x/.' 'a/b?x=1' 'a/b#c' '/b' 'a/' 'a/b$(id)' \
		'a/b`id`' 'a/b%2e' 'a\b' 'user@host/x'; do
		new_case "PCO_REPO '$bad' is refused"
		set_env PCO_REPO "$bad"
		run_install
		assert_fail "PCO_REPO must look like owner/name"
		assert_calls curl 0
		assert_nothing_installed
	done
	new_case "PCO_REPO names the repository of the downloads"
	set_env PCO_REPO some-one/else_repo.v2
	run_install
	assert_rc 0
	assert_log_has "https://api.github.com/repos/some-one/else_repo.v2/releases/latest"
	assert_log_has "https://github.com/some-one/else_repo.v2/releases/download/v1.2.3/$PKG"
	assert_stdout_has "release: https://github.com/some-one/else_repo.v2/releases/download/v1.2.3/"
}

case_curl_calls() {
	local line size
	new_case "every download ignores curlrc, is https only with TLS 1.2, times out and is capped in size"
	run_install
	assert_rc 0
	assert_calls curl 4
	while IFS= read -r line; do
		if starts_with "$line" "curl "; then
			size=$SMALL_FILE
			if [[ $line == *.deb ]]; then
				size=$PACKAGE_FILE
			fi
			ensure "curl call is as expected: $line" matches "$line" \
				"$CURL_FLAGS --max-filesize $size --output $CASE_DIR/tmp/pco-install.* https://*"
		fi
	done <<<"$LOG"

	new_case "a failed download of the package stops"
	set_cfg curl-fail-on ".deb"
	run_install
	assert_fail "download failed: $RELEASE_URL/$PKG (curl: (22)"
	assert_calls apt-get 0
	assert_tmp_gone
	assert_nothing_elsewhere

	new_case "a failed download of the checksums stops"
	set_cfg curl-fail-on "checksums.txt"
	run_install
	assert_fail "download failed: $RELEASE_URL/checksums.txt (curl: (22)"
	assert_nothing_installed

	new_case "a failed download of the signature stops"
	set_cfg curl-fail-on "checksums.txt.sig"
	run_install
	assert_fail "download failed: $RELEASE_URL/checksums.txt.sig (curl: (22)"
	assert_calls gpgv 0
	assert_nothing_installed
}

case_offline() {
	new_case "the offline variables install from local files without curl"
	use_local_files
	run_install
	assert_rc 0
	assert_calls curl 0
	assert_calls gpgv 1
	assert_calls apt-get 1
	assert_log_has "apt-get $APT_FLAGS $CASE_DIR/tmp/pco-install.*/$PKG"
	assert_stdout_has "source: local files, nothing is downloaded"
	assert_stdout_has "package: $PKG, architecture amd64"
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	assert_tmp_gone

	new_case "the offline lookup uses the base name of PCO_DEB"
	mkdir -p "$CASE_DIR/local/some/dir"
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/local/some/dir/pco_9.9.9_arm64.deb"
	printf '%s  pco_9.9.9_arm64.deb\n%s  %s\n' "$HASH_A" "$HASH_B" "$PKG" >"$CASE_DIR/local/sums"
	cp "$CASE_DIR/fixtures/checksums.txt.sig" "$CASE_DIR/local/sig"
	set_env PCO_DEB "$CASE_DIR/local/some/dir/pco_9.9.9_arm64.deb"
	set_env PCO_CHECKSUMS "$CASE_DIR/local/sums"
	set_env PCO_SIGNATURE "$CASE_DIR/local/sig"
	run_install
	assert_rc 0
	assert_calls curl 0
	assert_log_has "pco-install.*/pco_9.9.9_arm64.deb"

	new_case "offline, a checksum that does not match stops"
	use_local_files
	set_cfg sha256 "$HASH_B"
	run_install
	assert_fail "does not match checksums.txt"
	assert_nothing_installed

	new_case "offline, a bad signature stops"
	use_local_files
	set_cfg gpgv bad
	run_install
	assert_fail "signature of checksums.txt is not valid"
	assert_calls sha256sum 0
	assert_nothing_installed

	new_case "offline with the override needs no signature file"
	use_local_files
	rm "$CASE_DIR/local/checksums.txt.sig"
	CASE_ENV=("${CASE_ENV[@]/PCO_SIGNATURE=*/PCO_INSECURE_SKIP_SIGNATURE=1}")
	run_install
	assert_rc 0
	assert_calls gpgv 0
	assert_calls curl 0
	assert_stdout_has "checksum only (signature check skipped)"

	new_case "offline without a signature and without the override stops"
	use_local_files
	CASE_ENV=("${CASE_ENV[@]/PCO_SIGNATURE=*/PCO_SIGNATURE=}")
	run_install
	assert_fail "PCO_SIGNATURE is not set"
	assert_nothing_installed

	new_case "PCO_DEB without PCO_CHECKSUMS stops"
	set_env PCO_DEB "$CASE_DIR/local/$PKG"
	run_install
	assert_fail "PCO_DEB and PCO_CHECKSUMS must be set together"
	assert_calls curl 0

	new_case "PCO_SIGNATURE alone stops"
	set_env PCO_SIGNATURE "$CASE_DIR/local/sig"
	run_install
	assert_fail "PCO_DEB and PCO_CHECKSUMS must be set together"
	assert_calls curl 0

	new_case "a missing local file stops"
	use_local_files
	rm "$CASE_DIR/local/checksums.txt"
	run_install
	assert_fail "not a readable file: $CASE_DIR/local/checksums.txt"
	assert_nothing_installed

	new_case "a missing local signature stops"
	use_local_files
	rm "$CASE_DIR/local/checksums.txt.sig"
	run_install
	assert_fail "not a readable file: $CASE_DIR/local/checksums.txt.sig"
	assert_nothing_installed

	new_case "PCO_DEB must be a .deb with a plain name"
	use_local_files
	cp "$CASE_DIR/local/$PKG" "$CASE_DIR/local/pco package.deb"
	set_env PCO_DEB "$CASE_DIR/local/pco package.deb"
	run_install
	assert_fail "PCO_DEB must name a .deb file with a plain file name"
	assert_nothing_installed

	new_case "a relative PCO_DEB reaches apt-get as an absolute path in the temporary directory"
	use_local_files
	cp "$CASE_DIR/local/$PKG" "$CASE_DIR/work/$PKG"
	CASE_ENV=("${CASE_ENV[@]/PCO_DEB=*/PCO_DEB=$PKG}")
	run_install
	assert_rc 0
	assert_log_has "apt-get $APT_FLAGS $CASE_DIR/tmp/pco-install.*/$PKG"
	# The copy was installed; the original was only read.
	ensure "the file named by PCO_DEB is left alone" is_file "$CASE_DIR/work/$PKG"
}

case_install_call() {
	new_case "apt-get gets the verified package from a private directory, without prompts"
	run_install
	assert_rc 0
	assert_calls apt-get 1
	assert_log_has "apt-get $APT_FLAGS $CASE_DIR/tmp/pco-install.*/$PKG"
	assert_log_has "apt-get: DEBIAN_FRONTEND=noninteractive"
	assert_log_has "apt-get: package file exists"
	assert_log_has "apt-get: directory mode drwx------"

	new_case "a failing apt-get stops before pco setup"
	set_cfg apt-rc 100
	run_install
	assert_fail "apt-get could not install $PKG"
	assert_calls apt-get 1
	assert_calls pco 0
	assert_tmp_gone
}

case_hand_over() {
	new_case "the happy path installs once, then runs pco setup with the arguments"
	run_install --name shop --yes 'two words'
	assert_rc 0
	assert_stderr_empty
	assert_calls apt-get 1
	assert_calls pco 1
	assert_log_has "pco setup --name shop --yes two words"
	assert_before apt-get pco
	assert_tmp_gone
	assert_nothing_elsewhere

	new_case "the exit code of pco setup is the exit code of the script"
	set_cfg pco-rc 3
	run_install --yes
	assert_rc 3

	new_case "the temporary directory is gone before pco setup takes over"
	use_variant "$ROOT/usrbin/pco" "$CASE_DIR/tty"
	: >"$CASE_DIR/tty"
	set_env PCO_PROFILE host
	run_install
	assert_rc 0
	assert_log_has "pco: temporary directories left: 0"

	new_case "PCO_SKIP_SETUP=1 installs and stops"
	set_env PCO_SKIP_SETUP 1
	run_install --yes
	assert_rc 0
	assert_calls apt-get 1
	assert_calls pco 0
	assert_stdout_has "skipping the setup (PCO_SKIP_SETUP=1); run: pco setup"
	assert_tmp_gone

	new_case "without stdin on a terminal, pco setup reads from the terminal"
	printf 'typed answers' >"$CASE_DIR/tty"
	use_variant "$ROOT/usrbin/pco" "$CASE_DIR/tty"
	set_cfg pco-read-stdin 1
	set_env PCO_PROFILE host
	run_install
	assert_rc 0
	assert_log_has "pco setup"
	assert_log_has "pco: stdin holds: typed answers"
	assert_tmp_gone

	new_case "without any terminal, --yes goes on with the questions answered"
	set_cfg pco-read-stdin 1
	run_install --yes
	assert_rc 0
	assert_log_has "pco setup --yes"
	assert_log_lacks "pco setup --yes --yes"
	assert_log_has "pco: stdin holds:"
	assert_stdout_has "no terminal, starting pco setup with the answers of --yes"
	assert_tmp_gone

	new_case "without any terminal, -y counts as --yes"
	run_install -y
	assert_rc 0
	assert_log_has "pco setup -y"

	new_case "without any terminal and without --yes, it tells how to finish"
	run_install --name shop
	assert_rc 0
	assert_stderr_empty
	assert_calls apt-get 1
	assert_calls pco 0
	assert_stdout_has "to finish, run: pco setup"
	assert_tmp_gone

	new_case "a package that installed no pco is reported"
	use_variant "$CASE_DIR/no-such-binary" "$ROOT/no-tty"
	run_install
	assert_fail "$CASE_DIR/no-such-binary is missing or not executable after the install, so the setup cannot start"
	assert_calls apt-get 1
	assert_tmp_gone

	new_case "pco setup runs from the path the package installs, not from the PATH"
	add_stub pco
	run_install --yes
	assert_rc 0
	assert_calls pco 1
	assert_log_has "pco: run as $ROOT/usrbin/pco"
	assert_log_lacks "pco: run as $CASE_DIR/bin/pco"

	new_case "a pco on the PATH is not run when the installed one is missing"
	add_stub pco
	use_variant "$CASE_DIR/no-such-binary" "$ROOT/no-tty"
	run_install --yes
	assert_fail "is missing or not executable after the install"
	assert_calls pco 0
}

case_temporary_directory() {
	local value
	new_case "the directory is made in /tmp, whatever TMPDIR says"
	run_install
	assert_rc 0
	assert_calls mktemp 1
	assert_log_line "mktemp -d /tmp/pco-install.XXXXXXXX"
	assert_log_lacks "$CASE_DIR/untrusted"
	assert_nothing_elsewhere
	assert_tmp_gone

	for value in "" "relative/tmp" "/home/alice/tmp" "/tmp/../home/alice"; do
		new_case "TMPDIR '$value' is ignored"
		set_env TMPDIR "$value"
		run_install
		assert_rc 0
		assert_calls mktemp 1
		assert_log_line "mktemp -d /tmp/pco-install.XXXXXXXX"
	done

	new_case "a failing mktemp stops before any download"
	set_cfg mktemp-fail 1
	run_install
	assert_fail "cannot create a temporary directory in /tmp"
	assert_calls curl 0
	assert_nothing_installed
	assert_nothing_elsewhere

	new_case "TMPDIR is ignored when the script fails after the directory was made"
	set_cfg gpgv bad
	run_install
	assert_fail "signature of checksums.txt is not valid"
	assert_log_line "mktemp -d /tmp/pco-install.XXXXXXXX"
	assert_nothing_elsewhere
	assert_tmp_gone
}

case_hand_over_terminal() {
	new_case "with stdin on a terminal, pco setup gets it as it is"
	if ! command -v python3 >/dev/null 2>&1; then
		echo "skipped [$CASE_NAME]: no python3 to open a pseudo terminal"
		SKIPPED=$((SKIPPED + 1))
		return
	fi
	write_pty_runner
	RUN_PREFIX=(python3 "$ROOT/pty_run.py")
	run_install --yes
	assert_rc 0
	assert_log_has "pco setup --yes"
	assert_log_has "pco: stdin is a terminal"
	assert_tmp_gone
}

case_profile_question() {
	local answer

	new_case "without a choice and without a terminal it is the host profile"
	run_install --name shop --yes
	assert_rc 0
	assert_calls apt-get 1
	assert_calls dpkg-deb 0
	assert_log_has "pco setup --name shop --yes"
	assert_stdout_lacks "$QUESTION"

	new_case "on a terminal, Enter at the question keeps the host profile"
	use_terminal $'\n'
	run_install --name shop
	assert_rc 0
	assert_stdout_has "$QUESTION"
	assert_calls apt-get 1
	assert_calls dpkg-deb 0
	assert_log_has "pco setup --name shop"
	assert_tmp_gone

	for answer in host Host HOST h ' host '; do
		new_case "on a terminal, the answer '$answer' is the host profile"
		use_terminal "$answer"$'\n'
		run_install
		assert_rc 0
		assert_calls apt-get 1
		assert_calls dpkg-deb 0
		assert_log_has "pco setup"
	done

	for answer in appliance Appliance APPLIANCE a ' appliance '; do
		new_case "on a terminal, the answer '$answer' is the appliance profile"
		use_terminal "$answer"$'\n'
		run_install --vmid 120
		assert_rc 0
		assert_calls apt-get 0
		assert_calls dpkg-deb 1
		assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --vmid 120"
		assert_tmp_gone
	done

	new_case "on a terminal, the last answer may lack its newline"
	use_terminal appliance
	run_install
	assert_rc 0
	assert_calls dpkg-deb 1

	new_case "an answer that is neither is asked again"
	use_terminal $'maybe\nappliance\n'
	run_install
	assert_rc 0
	assert_stdout_has "answer host or appliance"
	assert_calls dpkg-deb 1

	new_case "a terminal that gives no answer stops before anything is downloaded"
	use_terminal ''
	run_install
	assert_fail "no answer to the question"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "the question is not asked when --appliance chose"
	use_terminal $'host\n'
	run_install --appliance
	assert_rc 0
	assert_stdout_lacks "$QUESTION"
	assert_calls dpkg-deb 1

	new_case "the question is not asked when --profile host chose"
	use_terminal $'appliance\n'
	run_install --profile host
	assert_rc 0
	assert_stdout_lacks "$QUESTION"
	assert_calls apt-get 1
	assert_calls dpkg-deb 0

	new_case "the question is not asked when PCO_PROFILE chose"
	use_terminal $'appliance\n'
	set_env PCO_PROFILE host
	run_install
	assert_rc 0
	assert_stdout_lacks "$QUESTION"
	assert_calls apt-get 1

	new_case "--yes takes the host profile without asking"
	use_terminal $'appliance\n'
	run_install --yes
	assert_rc 0
	assert_stdout_lacks "$QUESTION"
	assert_calls apt-get 1
	assert_calls dpkg-deb 0

	new_case "--uninstall without a choice asks nothing and is the host profile"
	use_terminal $'appliance\n'
	run_install --uninstall
	assert_fail "use pco uninstall"
	assert_stdout_lacks "$QUESTION"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "with stdin on a terminal, the question is asked there"
	if use_pty $'\n'; then
		run_install --name shop
		assert_rc 0
		assert_stdout_has "$QUESTION"
		assert_calls apt-get 1
		assert_calls dpkg-deb 0
		assert_log_has "pco setup --name shop"
		assert_log_has "pco: stdin is a terminal"
		assert_tmp_gone
	fi

	new_case "with stdin on a terminal, appliance typed at the question is the appliance profile"
	if use_pty $'appliance\n'; then
		run_install --vmid 120
		assert_rc 0
		assert_stdout_has "$QUESTION"
		assert_calls apt-get 0
		assert_calls dpkg-deb 1
		assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --vmid 120"
		assert_log_has "pco: stdin is a terminal"
		assert_tmp_gone
	fi
}

case_profile_choice() {
	local spec
	local -a choice

	for spec in "--appliance" "--profile appliance" "--profile=appliance"; do
		read -r -a choice <<<"$spec"
		new_case "$spec takes the appliance path and is not passed on"
		run_install "${choice[@]}" --yes --vmid 120
		assert_rc 0
		assert_calls apt-get 0
		assert_calls dpkg-deb 1
		assert_calls pco 1
		assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes --vmid 120"
		assert_log_lacks "--appliance"
		assert_log_lacks "--profile"
	done

	new_case "PCO_PROFILE=appliance takes the appliance path"
	set_env PCO_PROFILE appliance
	run_install --yes --vmid 120
	assert_rc 0
	assert_calls apt-get 0
	assert_calls dpkg-deb 1
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes --vmid 120"

	for spec in "--profile host" "--profile=host"; do
		read -r -a choice <<<"$spec"
		new_case "$spec takes the host path and is not passed on"
		run_install "${choice[@]}" --name shop --yes
		assert_rc 0
		assert_calls apt-get 1
		assert_calls dpkg-deb 0
		assert_log_has "pco setup --name shop --yes"
		assert_log_lacks "--profile"
	done

	new_case "PCO_PROFILE=host takes the host path"
	set_env PCO_PROFILE host
	run_install --name shop --yes
	assert_rc 0
	assert_calls apt-get 1
	assert_calls dpkg-deb 0

	new_case "an empty PCO_PROFILE counts as not set"
	set_env PCO_PROFILE ""
	run_install --name shop --yes
	assert_rc 0
	assert_calls apt-get 1

	new_case "an argument wins over PCO_PROFILE"
	set_env PCO_PROFILE host
	run_install --appliance --yes
	assert_rc 0
	assert_calls apt-get 0
	assert_calls dpkg-deb 1

	new_case "--yes --appliance takes the appliance path and never installs on the node"
	run_install --yes --appliance
	assert_rc 0
	assert_calls apt-get 0
	assert_calls dpkg-deb 1
	assert_calls pco 1
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes"
	assert_log_lacks "--appliance"

	for spec in "--yes --vmid 120 --profile appliance" "--vmid 120 --yes --profile=appliance"; do
		read -r -a choice <<<"$spec"
		new_case "$spec chooses the appliance wherever it stands and is not passed on"
		run_install "${choice[@]}"
		assert_rc 0
		assert_calls apt-get 0
		assert_calls dpkg-deb 1
		assert_log_has "--checksums $CASE_DIR/tmp/pco-install.*/checksums.txt ${spec/ --profile*/}"
		assert_log_lacks "--profile"
	done

	new_case "--profile host after other arguments chooses the host path and is not passed on"
	run_install --name shop --profile host --yes
	assert_rc 0
	assert_calls apt-get 1
	assert_calls dpkg-deb 0
	assert_log_has "pco setup --name shop --yes"
	assert_log_lacks "--profile"

	new_case "a profile after other arguments wins over PCO_PROFILE"
	set_env PCO_PROFILE host
	run_install --yes --appliance
	assert_rc 0
	assert_calls apt-get 0
	assert_calls dpkg-deb 1

	new_case "the arguments around a profile keep their order and their words"
	run_install --name 'two words' --appliance --yes --vmid 120
	assert_rc 0
	assert_log_has "--checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --name two words --yes --vmid 120"

	new_case "a profile named twice is the profile"
	run_install --appliance --profile appliance --yes
	assert_rc 0
	assert_calls dpkg-deb 1
	assert_log_lacks "--profile"

	new_case "PCO_PROFILE other than host or appliance is refused"
	set_env PCO_PROFILE vm
	run_install --yes
	assert_fail "PCO_PROFILE must be host or appliance, got vm"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "a refused PCO_PROFILE is refused even when an argument chooses"
	set_env PCO_PROFILE vm
	run_install --appliance --yes
	assert_fail "PCO_PROFILE must be host or appliance, got vm"
	assert_calls curl 0

	for spec in "--profile vm" "--profile=vm"; do
		read -r -a choice <<<"$spec"
		new_case "$spec is refused"
		run_install "${choice[@]}" --yes
		assert_fail "--profile must be host or appliance, got vm"
		assert_calls curl 0
		assert_nothing_unpacked
	done

	new_case "--profile without a value is refused"
	run_install --profile
	assert_fail "--profile needs a value"
	assert_calls curl 0

	new_case "--appliance with --profile host is refused"
	run_install --appliance --profile host --yes
	assert_fail "the arguments name both profiles"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "two profiles that are not next to each other are refused too"
	run_install --yes --appliance --vmid 120 --profile=host
	assert_fail "the arguments name both profiles"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "--profile as the last argument needs a value"
	run_install --yes --profile
	assert_fail "--profile needs a value"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "a profile that is refused after other arguments stops before anything is downloaded"
	run_install --yes --profile=vm
	assert_fail "--profile must be host or appliance, got vm"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "a refused argument shows a long value cut short"
	run_install --profile "$(printf 'x%.0s' {1..100})"
	assert_fail "--profile must be host or appliance, got xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx..."
}

case_appliance_install() {
	new_case "the appliance path unpacks the verified package and runs the installer from it"
	run_install --appliance --yes --vmid 120 'two words'
	assert_rc 0
	assert_stderr_empty
	assert_calls apt-get 0
	assert_calls dpkg-deb 1
	assert_calls pco 1
	assert_log_has "dpkg-deb -x $CASE_DIR/tmp/pco-install.*/$PKG $CASE_DIR/tmp/pco-install.*/root"
	assert_log_has "dpkg-deb: package file exists: yes"
	assert_log_has "pco: run as $CASE_DIR/tmp/pco-install.*/root/usr/bin/pco"
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes --vmid 120 two words"
	assert_log_has "pco: temporary directories left: 1"
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	assert_stdout_has "unpacking $PKG, nothing is installed on this node"
	assert_before gpgv sha256sum
	assert_before sha256sum dpkg-deb
	assert_before dpkg-deb pco
	ensure "the downloads are those of the host profile: $(curl_urls)" \
		equals "$(curl_urls)" "https://api.github.com/repos/$REPO/releases/latest $RELEASE_URL/$PKG $RELEASE_URL/checksums.txt $RELEASE_URL/checksums.txt.sig"
	assert_tmp_gone
	assert_nothing_elsewhere

	new_case "the appliance path does not need apt-get"
	drop_command apt-get
	run_install --appliance --yes
	assert_rc 0
	assert_calls dpkg-deb 1
	assert_calls pco 1

	new_case "the appliance path needs dpkg-deb, and the host path does not"
	drop_command dpkg-deb
	run_install --appliance --yes
	assert_fail "missing required command(s): dpkg-deb"
	assert_calls curl 0

	new_case "the host path needs no dpkg-deb"
	drop_command dpkg-deb
	run_install --yes
	assert_rc 0
	assert_calls apt-get 1

	new_case "the exit code of the installer is the exit code of the script"
	set_cfg pco-rc 3
	run_install --appliance --yes
	assert_rc 3
	assert_stderr_empty
	assert_tmp_gone

	new_case "the release the template comes from follows PCO_REPO"
	set_env PCO_REPO some-one/else_repo.v2
	run_install --appliance --yes
	assert_rc 0
	assert_log_has "--release-base https://github.com/some-one/else_repo.v2/releases/download/v1.2.3 --checksums"

	new_case "the release the template comes from follows PCO_VERSION"
	set_env PCO_VERSION 0.9.1
	cp "$CASE_DIR/fixtures/$PKG" "$CASE_DIR/fixtures/pco_0.9.1_amd64.deb"
	printf '%s  pco_0.9.1_amd64.deb\n' "$HASH_A" >"$CASE_DIR/fixtures/checksums.txt"
	run_install --appliance --yes
	assert_rc 0
	assert_log_has "--release-base https://github.com/$REPO/releases/download/v0.9.1 --checksums"

	new_case "offline, nothing is downloaded and no release is named"
	use_local_files
	run_install --appliance --yes
	assert_rc 0
	assert_calls curl 0
	assert_calls dpkg-deb 1
	assert_log_lacks "--release-base"
	assert_log_has "pco appliance install --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes"
	assert_stdout_has "source: local files, nothing is downloaded"

	new_case "a bad signature stops before the package is unpacked"
	set_cfg gpgv bad
	run_install --appliance --yes
	assert_fail "signature of checksums.txt is not valid"
	assert_nothing_unpacked
	assert_tmp_gone

	new_case "a checksum that does not match stops before the package is unpacked"
	set_cfg sha256 "$HASH_B"
	run_install --appliance --yes
	assert_fail "checksum of $PKG does not match checksums.txt"
	assert_nothing_unpacked
	assert_tmp_gone

	new_case "a package that cannot be unpacked stops before the installer"
	set_cfg dpkg-deb-fail 1
	run_install --appliance --yes
	assert_fail "dpkg-deb could not unpack $PKG (dpkg-deb: error: not a Debian format archive)"
	assert_calls pco 0
	assert_tmp_gone

	new_case "a package without pco stops before the installer"
	set_cfg dpkg-deb-no-pco 1
	run_install --appliance --yes
	assert_fail "$PKG holds no usr/bin/pco to run"
	assert_calls pco 0
	assert_tmp_gone

	new_case "the placeholder key refuses the appliance too"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	run_install --appliance --yes
	assert_fail "this build of the script carries no release key"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "the override of the signature goes for the appliance too, and says so"
	SCRIPT_UNDER_TEST=$PLACEHOLDER_SCRIPT
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	run_install --appliance --yes
	assert_rc 0
	assert_stderr_has "install.sh: WARNING: PCO_INSECURE_SKIP_SIGNATURE=1, the signature of checksums.txt is NOT checked"
	assert_stdout_has "verified by: checksum only (signature check skipped)"
	assert_calls dpkg-deb 1
}

case_appliance_input() {
	new_case "without stdin on a terminal, the installer reads from the terminal"
	use_terminal 'typed answers'
	set_cfg pco-read-stdin 1
	run_install --appliance
	assert_rc 0
	assert_log_has "pco: stdin holds: typed answers"
	assert_stdout_has "starting pco appliance install on the terminal"
	assert_tmp_gone

	new_case "without any terminal, --yes goes on"
	set_cfg pco-read-stdin 1
	run_install --appliance --yes
	assert_rc 0
	assert_log_has "pco: stdin holds:"
	assert_stdout_has "no terminal, starting pco appliance install with the answers of --yes"

	new_case "without any terminal, -y counts as --yes"
	run_install --appliance -y
	assert_rc 0
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt -y"

	new_case "without a terminal and without --yes it stops before anything is downloaded"
	run_install --appliance --vmid 120
	assert_fail "no terminal to ask the installer's questions on: run it from a terminal, or add --yes"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "with stdin on a terminal, the installer gets it as it is"
	if use_pty ''; then
		run_install --appliance
		assert_rc 0
		assert_log_has "pco appliance install"
		assert_log_has "pco: stdin is a terminal"
		assert_stdout_has "starting pco appliance install"
		assert_tmp_gone
	fi
}

case_appliance_template() {
	new_case "PCO_TEMPLATE is checked against checksums.txt and passed on"
	use_template
	run_install --appliance --yes
	assert_rc 0
	assert_log_has "sha256sum $CASE_DIR/local/$TEMPLATE"
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --template $CASE_DIR/local/$TEMPLATE --yes"
	assert_stdout_has "checking the checksum of $TEMPLATE"
	assert_before sha256sum dpkg-deb
	assert_before sha256sum pco
	assert_tmp_gone

	new_case "offline, with a template of its own, nothing is downloaded"
	use_template
	use_local_files
	run_install --appliance --yes
	assert_rc 0
	assert_calls curl 0
	assert_log_lacks "--release-base"
	assert_log_has "pco appliance install --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --template $CASE_DIR/local/$TEMPLATE --yes"

	new_case "a relative PCO_TEMPLATE is passed on as an absolute path"
	use_template
	cp "$CASE_DIR/local/$TEMPLATE" "$CASE_DIR/work/$TEMPLATE"
	CASE_ENV=("${CASE_ENV[@]/PCO_TEMPLATE=*/PCO_TEMPLATE=$TEMPLATE}")
	run_install --appliance --yes
	assert_rc 0
	# The script starts with no PWD, so it is where the system says the directory is.
	assert_log_has "--template $(cd "$CASE_DIR/work" && pwd -P)/$TEMPLATE --yes"
	ensure "the file named by PCO_TEMPLATE is left alone" is_file "$CASE_DIR/work/$TEMPLATE"

	new_case "a template that does not match checksums.txt stops before the hand-over"
	use_template
	set_cfg "sha256-$TEMPLATE" "$HASH_B"
	run_install --appliance --yes
	assert_fail "checksum of $TEMPLATE does not match checksums.txt: expected $HASH_C, got $HASH_B"
	assert_nothing_unpacked
	assert_tmp_gone

	new_case "a template that checksums.txt does not name stops before the hand-over"
	use_template
	printf '%s  %s\n' "$HASH_A" "$PKG" >"$CASE_DIR/fixtures/checksums.txt"
	run_install --appliance --yes
	assert_fail "checksums.txt has no line for $TEMPLATE"
	assert_nothing_unpacked
	assert_tmp_gone

	new_case "a template of another name is not the one checksums.txt names"
	use_template
	cp "$CASE_DIR/local/$TEMPLATE" "$CASE_DIR/local/other.tar.zst"
	set_env PCO_TEMPLATE "$CASE_DIR/local/other.tar.zst"
	run_install --appliance --yes
	assert_fail "checksums.txt has no line for other.tar.zst"
	assert_nothing_unpacked

	new_case "a bad signature stops before the template is looked at"
	use_template
	set_cfg gpgv bad
	run_install --appliance --yes
	assert_fail "signature of checksums.txt is not valid"
	assert_calls sha256sum 0
	assert_nothing_unpacked

	new_case "a template that is not a file stops before anything is downloaded"
	use_template
	rm "$CASE_DIR/local/$TEMPLATE"
	run_install --appliance --yes
	assert_fail "not a readable file: $CASE_DIR/local/$TEMPLATE"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "PCO_TEMPLATE must be a .tar.zst with a plain name"
	use_template
	cp "$CASE_DIR/local/$TEMPLATE" "$CASE_DIR/local/pco template.tar.zst"
	set_env PCO_TEMPLATE "$CASE_DIR/local/pco template.tar.zst"
	run_install --appliance --yes
	assert_fail "PCO_TEMPLATE must name a .tar.zst file with a plain file name"
	assert_calls curl 0

	new_case "PCO_TEMPLATE is for the appliance profile"
	use_template
	run_install --yes
	assert_fail "PCO_TEMPLATE is for the appliance profile"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "without PCO_TEMPLATE no template is passed on"
	run_install --appliance --yes
	assert_rc 0
	assert_log_lacks "--template"

	new_case "--uninstall has no use for PCO_TEMPLATE and does not check it"
	use_template
	run_install --appliance --uninstall --yes --vmid 120
	assert_rc 0
	assert_calls sha256sum 1
	assert_log_lacks "--template"
}

case_appliance_failure() {
	local spec sig status

	new_case "a failing installer leaves no temporary directory and offers its journal"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	set_cfg pco-rc 1
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 1
	assert_tmp_gone
	assert_stderr_has "the installer did not finish, and left its journal in $CASE_DIR/journals"
	assert_stderr_has "to finish the run or take it back, run: PCO_PROFILE=appliance PCO_VERSION=1.2.3 PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"
	assert_stderr_has "install.sh stands for the script as you ran it"
	assert_nothing_elsewhere

	new_case "a failing installer that left no journal offers nothing"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	set_cfg pco-rc 1
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_empty
	assert_tmp_gone

	new_case "a journal of an earlier run is not offered"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal 20260101T000000-0000.json
	set_cfg pco-rc 1
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_has "PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"
	assert_stderr_lacks "20260101T000000-0000.json"

	new_case "an installer that succeeds offers nothing"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 0
	assert_stderr_empty

	new_case "offline, the offer does not name a release"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	use_local_files
	set_cfg pco-rc 1
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_has "PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"
	assert_stderr_lacks "PCO_VERSION"
	assert_stderr_has "run: PCO_PROFILE=appliance PCO_DEB=$CASE_DIR/local/$PKG PCO_CHECKSUMS=$CASE_DIR/local/checksums.txt PCO_SIGNATURE=$CASE_DIR/local/checksums.txt.sig PCO_RESUME="

	new_case "the offer names every variable the run was made with"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	use_template
	set_env PCO_REPO some-one/else_repo.v2
	set_env PCO_INSECURE_SKIP_SIGNATURE 1
	set_cfg pco-rc 1
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_has "run: PCO_PROFILE=appliance PCO_VERSION=1.2.3 PCO_REPO=some-one/else_repo.v2 PCO_TEMPLATE=$CASE_DIR/local/$TEMPLATE PCO_INSECURE_SKIP_SIGNATURE=1 PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"

	new_case "the line of the offer, run as printed, resumes the run"
	use_terminal 'typed answers'
	set_cfg pco-rc 1
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --yes
	assert_rc 1
	use_printed "$(printed_after "$ERR" "run: ")"
	# What the latest release is by now is not the release of the run.
	printf '{\n  "tag_name": "v9.9.9"\n}\n' >"$CASE_DIR/cfg/api-response"
	set_cfg pco-rc 0
	run_install ${PRINTED_ARGS[@]+"${PRINTED_ARGS[@]}"}
	assert_rc 0
	assert_stderr_empty
	assert_calls apt-get 0
	assert_log_has "pco appliance install --resume $CASE_DIR/journals/$JOURNAL --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt"
	assert_tmp_gone

	new_case "a failure before the installer starts offers nothing"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal "$JOURNAL"
	set_cfg dpkg-deb-fail 1
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_lacks "PCO_RESUME"

	for spec in HUP:129 INT:130 TERM:143; do
		sig=${spec%%:*}
		status=${spec##*:}
		new_case "SIG$sig for the script is handled when the installer returns, and the directory goes"
		use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
		set_cfg pco-signal-parent "$sig"
		set_cfg pco-journal "$JOURNAL"
		set_cfg pco-rc 1
		run_install --appliance --yes
		assert_rc "$status"
		assert_tmp_gone
		assert_stderr_has "PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"
		assert_nothing_elsewhere
	done
}

case_appliance_resume() {
	new_case "PCO_RESUME hands the journal to --resume, and the package is fetched as always"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal "$JOURNAL"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	run_install --appliance --yes
	assert_rc 0
	assert_log_has "pco appliance install --resume $CASE_DIR/journals/$JOURNAL --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes"
	assert_calls curl 4
	assert_calls dpkg-deb 1
	assert_stderr_empty
	assert_tmp_gone

	new_case "offline, PCO_RESUME downloads nothing"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal "$JOURNAL"
	use_local_files
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	run_install --appliance --yes
	assert_rc 0
	assert_calls curl 0
	assert_log_has "pco appliance install --resume $CASE_DIR/journals/$JOURNAL --checksums"

	new_case "a resumed run that fails offers its journal again"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal "$JOURNAL"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	set_cfg pco-rc 1
	run_install --appliance --yes
	assert_rc 1
	assert_stderr_has "PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh"
	assert_tmp_gone

	new_case "PCO_RESUME that names no file stops before anything is downloaded"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	run_install --appliance --yes
	assert_fail "not a readable file: $CASE_DIR/journals/$JOURNAL"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "PCO_RESUME must be an absolute path"
	set_env PCO_RESUME "journals/$JOURNAL"
	run_install --appliance --yes
	assert_fail "PCO_RESUME must be the absolute path of a journal, got journals/$JOURNAL"
	assert_calls curl 0

	new_case "PCO_RESUME is for the appliance profile"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	run_install --yes
	assert_fail "PCO_RESUME is for the appliance profile"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "PCO_RESUME does not go with --uninstall"
	leave_journal "$JOURNAL"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	run_install --appliance --uninstall --yes --vmid 120
	assert_fail "PCO_RESUME finishes an install, it does not go with --uninstall"
	assert_calls curl 0
}

case_appliance_uninstall() {
	local spec
	local -a lead

	for spec in "--appliance --uninstall" "--uninstall --appliance" "--profile appliance --uninstall"; do
		read -r -a lead <<<"$spec"
		new_case "$spec runs pco appliance uninstall from the verified package"
		run_install "${lead[@]}" --vmid 120 --yes --keep-cloudflare
		assert_rc 0
		assert_calls apt-get 0
		assert_calls dpkg-deb 1
		assert_calls pco 1
		assert_log_has "pco appliance uninstall --vmid 120 --yes --keep-cloudflare"
		assert_log_lacks "--release-base"
		assert_log_lacks "--checksums"
		assert_log_lacks "--uninstall"
		assert_log_has "pco: run as $CASE_DIR/tmp/pco-install.*/root/usr/bin/pco"
		assert_before sha256sum dpkg-deb
		assert_before dpkg-deb pco
		assert_stdout_has "starting pco appliance uninstall with the answers of --yes"
		assert_tmp_gone
	done

	new_case "PCO_PROFILE=appliance with --uninstall"
	set_env PCO_PROFILE appliance
	run_install --uninstall --vmid 120 --yes
	assert_rc 0
	assert_log_has "pco appliance uninstall --vmid 120 --yes"

	new_case "--uninstall after other arguments is not passed on, and the appliance profile takes it"
	run_install --vmid 120 --yes --uninstall --appliance
	assert_rc 0
	assert_calls apt-get 0
	assert_log_has "pco appliance uninstall --vmid 120 --yes"
	assert_log_lacks "--uninstall --"
	assert_log_lacks "install --release-base"

	new_case "--uninstall after other arguments, with the host profile, points at pco uninstall"
	run_install --yes --uninstall
	assert_fail "use pco uninstall"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "an uninstall asks on the terminal, as the installer does"
	use_terminal 'typed answers'
	set_cfg pco-read-stdin 1
	run_install --appliance --uninstall --vmid 120
	assert_rc 0
	assert_log_has "pco: stdin holds: typed answers"
	assert_stdout_has "starting pco appliance uninstall on the terminal"

	new_case "an uninstall without a terminal and without --yes stops before anything is downloaded"
	run_install --appliance --uninstall --vmid 120
	assert_fail "no terminal to ask the installer's questions on"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "an uninstall is verified as an install is"
	set_cfg gpgv bad
	run_install --appliance --uninstall --vmid 120 --yes
	assert_fail "signature of checksums.txt is not valid"
	assert_nothing_unpacked
	assert_tmp_gone

	new_case "the exit code of the uninstaller is the exit code of the script, and it offers no journal"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	set_cfg pco-rc 2
	set_cfg pco-journal "$JOURNAL"
	run_install --appliance --uninstall --vmid 120 --yes
	assert_rc 2
	assert_stderr_empty
	assert_tmp_gone

	new_case "with the host profile --uninstall points at pco uninstall"
	run_install --uninstall --yes
	assert_fail "use pco uninstall to take a host install off this node; an appliance is removed with install.sh --appliance --uninstall --vmid <vmid>"
	assert_calls curl 0
	assert_nothing_unpacked

	new_case "--profile host with --uninstall points at pco uninstall too"
	run_install --profile host --uninstall --yes
	assert_fail "use pco uninstall"
	assert_calls curl 0
}

case_appliance_skip_setup() {
	new_case "PCO_SKIP_SETUP=1 verifies, prints the command to go on and stops, without a terminal"
	set_env PCO_SKIP_SETUP 1
	run_install --appliance --yes --vmid 120
	assert_rc 0
	assert_stderr_empty
	assert_nothing_unpacked
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	assert_stdout_has "skipping the appliance install (PCO_SKIP_SETUP=1); the package is verified and nothing was installed; to go on, run again without PCO_SKIP_SETUP: PCO_PROFILE=appliance PCO_VERSION=1.2.3 install.sh --yes --vmid 120"
	assert_stdout_has "install.sh stands for the script as you ran it"
	assert_stdout_lacks "$CASE_DIR/tmp"
	assert_stdout_lacks "pco appliance"
	assert_tmp_gone
	# The command, run as printed, goes on to the installer.
	use_printed "$(printed_after "$OUT" "without PCO_SKIP_SETUP: ")"
	printf '{\n  "tag_name": "v9.9.9"\n}\n' >"$CASE_DIR/cfg/api-response"
	run_install ${PRINTED_ARGS[@]+"${PRINTED_ARGS[@]}"}
	assert_rc 0
	assert_calls apt-get 0
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --yes --vmid 120"
	assert_tmp_gone

	new_case "PCO_SKIP_SETUP=1 with a template names it in the command to go on"
	use_template
	set_env PCO_SKIP_SETUP 1
	run_install --appliance --yes
	assert_rc 0
	assert_nothing_unpacked
	assert_stdout_has "run again without PCO_SKIP_SETUP: PCO_PROFILE=appliance PCO_VERSION=1.2.3 PCO_TEMPLATE=$CASE_DIR/local/$TEMPLATE install.sh --yes"
	use_printed "$(printed_after "$OUT" "without PCO_SKIP_SETUP: ")"
	run_install ${PRINTED_ARGS[@]+"${PRINTED_ARGS[@]}"}
	assert_rc 0
	assert_log_has "pco appliance install --release-base $RELEASE_URL --checksums $CASE_DIR/tmp/pco-install.*/checksums.txt --template $CASE_DIR/local/$TEMPLATE --yes"

	new_case "PCO_SKIP_SETUP=1 with PCO_RESUME keeps the journal in the command to go on"
	use_variant "$ROOT/usrbin/pco" "$ROOT/no-tty"
	leave_journal "$JOURNAL"
	set_env PCO_RESUME "$CASE_DIR/journals/$JOURNAL"
	set_env PCO_SKIP_SETUP 1
	run_install --appliance --yes
	assert_rc 0
	assert_nothing_unpacked
	assert_stdout_has "run again without PCO_SKIP_SETUP: PCO_PROFILE=appliance PCO_VERSION=1.2.3 PCO_RESUME=$CASE_DIR/journals/$JOURNAL install.sh --yes"

	new_case "PCO_SKIP_SETUP=1 stops an uninstall after the verification too"
	set_env PCO_SKIP_SETUP 1
	run_install --appliance --uninstall --yes --vmid 120
	assert_rc 0
	assert_nothing_unpacked
	assert_stdout_has "skipping the appliance uninstall (PCO_SKIP_SETUP=1); the package is verified and nothing was installed; to go on, run again without PCO_SKIP_SETUP: PCO_PROFILE=appliance PCO_VERSION=1.2.3 install.sh --uninstall --yes --vmid 120"
	assert_tmp_gone
	use_printed "$(printed_after "$OUT" "without PCO_SKIP_SETUP: ")"
	run_install ${PRINTED_ARGS[@]+"${PRINTED_ARGS[@]}"}
	assert_rc 0
	assert_log_has "pco appliance uninstall --yes --vmid 120"
	assert_log_lacks "--release-base"

	new_case "PCO_SKIP_SETUP=1 does not hide a failed check"
	set_env PCO_SKIP_SETUP 1
	set_cfg sha256 "$HASH_B"
	run_install --appliance
	assert_fail "does not match checksums.txt"
}

case_progress_messages() {
	new_case "it says what it is about to install and what verified it, before installing"
	run_install
	assert_rc 0
	assert_stdout_has "package: pco 1.2.3, architecture amd64"
	assert_stdout_has "release: $RELEASE_URL/"
	assert_stdout_has "verified by: signature by key $STUB_FPR and checksum"
	ensure "the verification is reported before the installation starts" matches "$OUT" "*verified by:*installing $PKG*"
	assert_stderr_empty
	ensure "no escape sequences in the output" omits "$OUT" $'\033'
}

case_structure() {
	local last total n line
	new_case "the last line is { main \"\$@\"; }"
	last=
	total=0
	while IFS= read -r line || [[ -n $line ]]; do
		last=$line
		total=$((total + 1))
	done <"$INSTALL"
	# shellcheck disable=SC2016 # the line is compared as text, nothing is meant to expand
	ensure "the last line is { main \"\$@\"; }, got '$last'" equals "$last" '{ main "$@"; }'

	new_case "a PATH of stubs notices a script that runs"
	only_stubs
	SCRIPT_UNDER_TEST=$INSTALL
	run_install
	ensure "the whole script ran nothing" differs "$LOG" ""

	new_case "a script truncated at a line boundary before main does nothing"
	only_stubs
	for ((n = 1; n < total; n++)); do
		head -n "$n" "$INSTALL" >"$CASE_DIR/truncated.sh"
		SCRIPT_UNDER_TEST=$CASE_DIR/truncated.sh
		run_install
		ensure "the copy cut after line $n ran something: $OUT $LOG" equals "$OUT$LOG" ""
	done

	new_case "a cut inside the last line is a syntax error and does nothing"
	only_stubs
	head -n $((total - 1)) "$INSTALL" >"$CASE_DIR/head.sh"
	for ((n = 1; n < ${#last}; n++)); do
		{
			cat "$CASE_DIR/head.sh"
			printf '%s' "${last:0:n}"
		} >"$CASE_DIR/truncated.sh"
		SCRIPT_UNDER_TEST=$CASE_DIR/truncated.sh
		run_install
		ensure "the copy cut after $n characters of the last line ran something: $OUT $LOG" equals "$OUT$LOG" ""
		ensure "bash accepts the copy cut after $n characters of the last line" differs "$RC" 0
	done

	new_case "the script starts with the shebang and strict mode"
	ensure "the first line is the bash shebang" equals "$(head -n 1 "$INSTALL")" '#!/usr/bin/env bash'
	ensure "the script sets -euo pipefail" grep -q '^set -euo pipefail$' "$INSTALL"
	ensure "the script never uses eval" lacks_eval

	new_case "the script takes the installed binary, the terminal and the journals from no variable"
	ensure "the binary is named by its path" grep -qx 'PCO_BIN=/usr/bin/pco' "$INSTALL"
	ensure "the terminal is named by its path" grep -qx 'TTY_DEVICE=/dev/tty' "$INSTALL"
	ensure "the journals are named by their directory" grep -qx 'JOURNAL_DIR=/root/.pco-appliance-install' "$INSTALL"
	# shellcheck disable=SC2016 # grep looks for the text, nothing is meant to expand
	ensure "the hand-over goes through that path" grep -qF 'exec "$PCO_BIN" setup' "$INSTALL"
	# shellcheck disable=SC2016 # grep looks for the text, nothing is meant to expand
	ensure "the appliance installer runs from the unpacked package" grep -qF '"$TMP_DIR/root/usr/bin/pco" appliance' "$INSTALL"
}

lacks_eval() {
	! grep -Eq '(^|[^[:alnum:]_])eval([^[:alnum:]_]|$)' "$INSTALL"
}

# Stubs for every command the script could run, the real tools included: a
# script that runs anything at all leaves a line in the call log, and nothing
# else is on the PATH to run.
only_stubs() {
	local t
	rm -f "$CASE_DIR"/bin/*
	for t in id pveversion dpkg dpkg-deb curl sha256sum gpgv sqv apt-get pco mktemp base64 cp rm cat ls sed grep awk tr \
		head tail dirname basename mkdir touch mv ln chmod date uname; do
		add_stub "$t"
	done
}

gpg_cmd() {
	GNUPGHOME=$GPG_HOME gpg --batch --quiet "$@"
}

# With PCO_REQUIRE_CRYPTO_TESTS=1 a missing tool fails the run instead of
# skipping the checks, so that CI cannot lose them unnoticed.
crypto_required() {
	[[ ${PCO_REQUIRE_CRYPTO_TESTS:-} == 1 ]]
}

skip_crypto() {
	if crypto_required; then
		CHECKS=$((CHECKS + 1))
		FAILS=$((FAILS + 1))
		printf 'FAIL [the real signature checks] PCO_REQUIRE_CRYPTO_TESTS=1 but %s\n' "$1"
		return 1
	fi
	SKIPPED=$((SKIPPED + 1))
	printf 'skipped [the real signature checks]: %s\n' "$1"
}

case_crypto_switch() {
	local out rc
	new_case "PCO_REQUIRE_CRYPTO_TESTS=1 turns a skipped crypto check into a failure"
	# Subshells: the counters of the run stay as they are.
	out=$(
		export PCO_REQUIRE_CRYPTO_TESTS=1
		skip_crypto "gpg is not installed"
	)
	rc=$?
	ensure "the strict switch fails the run" equals "$rc" 1
	ensure "the strict switch says what was missing" contains "$out" "FAIL [the real signature checks] PCO_REQUIRE_CRYPTO_TESTS=1 but gpg is not installed"
	out=$(
		unset PCO_REQUIRE_CRYPTO_TESTS
		skip_crypto "gpg is not installed"
	)
	rc=$?
	ensure "without the switch a skip is not a failure" equals "$rc" 0
	ensure "without the switch a skip is reported" contains "$out" "skipped [the real signature checks]: gpg is not installed"
}

# sha256 of a file with whatever tool the machine has.
real_sha256() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${sum%% *}"
}

# The lines of the key block of the real install.sh, between its header and the
# EOF that closes it.
real_key_block() {
	local line inside=0
	while IFS= read -r line; do
		if [[ $inside == 1 ]]; then
			if [[ $line == EOF ]]; then
				return 0
			fi
			printf '%s\n' "$line"
		elif [[ $line == "$KEY_HEADER" ]]; then
			inside=1
		fi
	done <"$INSTALL"
}

# The fingerprint of every primary key of a keyring, one per line.
primary_fingerprints() {
	GNUPGHOME=$1 gpg --batch --show-keys --with-colons "$2" 2>/dev/null |
		awk -F: '$1 == "pub" { want = 1; next } $1 == "sub" { want = 0 } $1 == "fpr" && want { print $10; want = 0 }'
}

# What install.sh trusts as it stands in the repository, not the keys the other
# cases make: the block decodes, and the keyring holds the release keys and no
# other primary key. A signature by that key cannot be made here, the private
# half is not in the repository, so it is the fingerprint that is pinned.
case_embedded_key() {
	local block home keyring fprs expected
	new_case "the key block of install.sh holds the release key"
	if ! command -v gpg >/dev/null 2>&1; then
		skip_crypto "gpg is not installed"
		return
	fi
	home=$CASE_DIR/gnupg
	keyring=$CASE_DIR/release.gpg
	mkdir -m 700 "$home"
	block=$(real_key_block)
	ensure "the key block is not empty" differs "$block" ""
	ensure "the key block is not the placeholder" differs "$block" "$PLACEHOLDER"
	printf '%s\n' "$block" | base64 -d >"$keyring" 2>/dev/null
	ensure "the key block is valid base64" equals "$?" 0
	fprs=$(primary_fingerprints "$home" "$keyring")
	expected=$(printf '%s\n' "${RELEASE_FPRS[@]}")
	ensure "the keyring holds the release key and no other" equals "$fprs" "$expected"
}

# Makes a key in the throwaway keyring and prints its fingerprint. More
# arguments go to gpg, for a key made at another time.
make_real_key() {
	local name=$1 email=$2 expiry=$3
	shift 3
	gpg_cmd "$@" --passphrase '' --quick-generate-key "$name Test <$email>" ed25519 sign "$expiry" 2>/dev/null
	gpg_cmd --with-colons --fingerprint "$email" | awk -F: '$1 == "fpr" { print $10; exit }'
}

sign_checksums() {
	local fpr=$1 out=$2
	shift 2
	gpg_cmd "$@" --yes --default-key "$fpr" --detach-sign --output "$out" "$ROOT/real/checksums.txt" 2>/dev/null
}

# Leaves the public key of FPR at $ROOT/real/NAME.gpg and a copy of the script
# that carries it at $ROOT/real/install-NAME.sh.
install_real_keyring() {
	local name=$1 fpr=$2 key
	gpg_cmd --export "$fpr" >"$ROOT/real/$name.gpg"
	# Wrapped like a key pasted into the script by hand.
	key=$(base64 <"$ROOT/real/$name.gpg" | tr -d '\n' | fold -w 64)
	make_variant_script "$ROOT/real/install-$name.sh" "$key" "$ROOT/usrbin/pco" "$ROOT/no-tty"
}

# Whether gpgv, left to itself, trusts the signature through the keys in
# $1/.gnupg, where it looks when it is given no keyring.
trusts_through_home() {
	env -i "HOME=$1" "GNUPGHOME=$1/.gnupg" "$(command -v gpgv)" "$2" "$3" >/dev/null 2>&1
}

# A case that runs the script offline with the real verifier, the real
# sha256sum and a script carrying the public key of KEY, on checksums signed
# as SIGNER.
real_signature_case() {
	local tool=$1 description=$2 key=$3 signer=$4
	new_case "[$tool] $description"
	SCRIPT_UNDER_TEST=$ROOT/real/install-$key.sh
	drop_command gpgv
	use_real "$tool"
	if command -v sha256sum >/dev/null 2>&1; then
		use_real sha256sum
	else
		printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' >"$CASE_DIR/bin/sha256sum"
		chmod +x "$CASE_DIR/bin/sha256sum"
	fi
	printf 'deb-bytes' >"$CASE_DIR/local/$PKG"
	cp "$ROOT/real/checksums.txt" "$CASE_DIR/local/checksums.txt"
	cp "$ROOT/real/$signer.sig" "$CASE_DIR/local/checksums.txt.sig"
	set_env PCO_DEB "$CASE_DIR/local/$PKG"
	set_env PCO_CHECKSUMS "$CASE_DIR/local/checksums.txt"
	set_env PCO_SIGNATURE "$CASE_DIR/local/checksums.txt.sig"
}

case_real_signature() {
	local tools=() tool fpr_good fpr_other fpr_revoked fpr_expired
	if ! command -v gpg >/dev/null 2>&1; then
		skip_crypto "gpg is not installed"
		return
	fi
	for tool in gpgv sqv; do
		if command -v "$tool" >/dev/null 2>&1; then
			tools+=("$tool")
		elif crypto_required; then
			skip_crypto "$tool is not installed"
			return
		fi
	done
	if [[ ${#tools[@]} == 0 ]]; then
		skip_crypto "neither gpgv nor sqv is installed"
		return
	fi
	if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
		skip_crypto "neither sha256sum nor shasum is installed"
		return
	fi

	# A short path: gpg-agent's socket must fit into the limit of the system.
	GPG_HOME=$(mktemp -d /tmp/pco-gpg.XXXXXX)
	chmod 700 "$GPG_HOME"
	mkdir "$ROOT/real"
	printf 'deb-bytes' >"$ROOT/real/deb"
	printf '%s  %s\n' "$(real_sha256 "$ROOT/real/deb")" "$PKG" >"$ROOT/real/checksums.txt"

	fpr_good=$(make_real_key Release release@example.invalid never)
	fpr_other=$(make_real_key Other other@example.invalid never)
	fpr_revoked=$(make_real_key Revoked revoked@example.invalid never)
	fpr_expired=$(make_real_key Expired expired@example.invalid 1d --faked-system-time 20200101T000000)
	sign_checksums "$fpr_good" "$ROOT/real/good.sig"
	sign_checksums "$fpr_other" "$ROOT/real/other.sig"
	sign_checksums "$fpr_revoked" "$ROOT/real/revoked.sig"
	sign_checksums "$fpr_expired" "$ROOT/real/expired.sig" --faked-system-time 20200101T120000
	# The signature was made while the key was fine; the key is revoked after.
	printf 'revkey\ny\n0\n\ny\nsave\n' |
		gpg_cmd --command-fd 0 --pinentry-mode loopback --passphrase '' --edit-key "$fpr_revoked" >/dev/null 2>&1
	install_real_keyring good "$fpr_good"
	install_real_keyring other "$fpr_other"
	install_real_keyring revoked "$fpr_revoked"
	install_real_keyring expired "$fpr_expired"

	for tool in "${tools[@]}"; do
		real_signature_case "$tool" "a good signature passes, with the real $tool and sha256sum" good good
		run_install
		assert_rc 0
		assert_stdout_has "verified by: signature by key $fpr_good and checksum"
		assert_calls apt-get 1
		assert_nothing_elsewhere

		real_signature_case "$tool" "a tampered checksums file fails the signature" good good
		printf '%s  %s\n' "$HASH_B" "$PKG" >"$CASE_DIR/local/checksums.txt"
		run_install
		assert_fail "signature of checksums.txt is not valid for the release key"
		assert_nothing_installed
		assert_tmp_gone

		real_signature_case "$tool" "a signature by another key fails" good other
		run_install
		assert_fail "signature of checksums.txt is not valid for the release key"
		assert_nothing_installed

		real_signature_case "$tool" "a signed checksums file that does not match the package fails" good good
		printf 'other deb bytes' >"$CASE_DIR/local/$PKG"
		run_install
		assert_fail "does not match checksums.txt"
		assert_nothing_installed

		real_signature_case "$tool" "a signature by a revoked key fails" revoked revoked
		run_install
		assert_fail "signature of checksums.txt is not valid for the release key"
		assert_nothing_installed
		assert_tmp_gone

		# sqv judges a key at the time of the signature, which was within its
		# validity, so this case is gpgv's alone.
		if [[ $tool == gpgv ]]; then
			real_signature_case "$tool" "a signature by an expired key fails" expired expired
			run_install
			assert_fail "signature of checksums.txt is not valid for the release key"
			assert_nothing_installed

			real_signature_case "$tool" "a key trusted in ~/.gnupg/trustedkeys.kbx does not count" good other
			mkdir -m 700 "$CASE_DIR/home/.gnupg"
			gpg_cmd --no-default-keyring --keyring "$CASE_DIR/home/.gnupg/trustedkeys.kbx" --import "$ROOT/real/other.gpg" 2>/dev/null
			ensure "gpgv trusts that key through the home directory" \
				trusts_through_home "$CASE_DIR/home" "$CASE_DIR/local/checksums.txt.sig" "$CASE_DIR/local/checksums.txt"
			# The same keys are what gpgv would use on its own in the script's run, too.
			set_env GNUPGHOME "$CASE_DIR/home/.gnupg"
			run_install
			assert_fail "signature of checksums.txt is not valid for the release key"
			assert_nothing_installed
			assert_tmp_gone
		fi
	done
}

write_stub
mkdir "$ROOT/usrbin"
ln -s "$ROOT/stub" "$ROOT/usrbin/pco"
KEYED=$ROOT/install-keyed.sh
make_variant_script "$KEYED" "$(fake_key)" "$ROOT/usrbin/pco" "$ROOT/no-tty"
PLACEHOLDER_SCRIPT=$ROOT/install-placeholder.sh
make_variant_script "$PLACEHOLDER_SCRIPT" "$PLACEHOLDER" "$ROOT/usrbin/pco" "$ROOT/no-tty"

case_not_root
case_no_pveversion
case_arch
case_preflight_order
case_required_commands
case_verifier_choice
case_placeholder_key
case_override_with_a_key
case_bad_key_block
case_bad_signature
case_gpgv_statuses
case_signature_call
case_checksum
case_latest_version
case_pinned_version
case_refused_versions
case_version_length
case_repo
case_curl_calls
case_offline
case_install_call
case_temporary_directory
case_hand_over
case_hand_over_terminal
case_profile_question
case_profile_choice
case_appliance_install
case_appliance_input
case_appliance_template
case_appliance_failure
case_appliance_resume
case_appliance_uninstall
case_appliance_skip_setup
case_progress_messages
case_structure
case_crypto_switch
case_embedded_key
case_real_signature

printf '%d cases, %d checks, %d failed, %d skipped (bash %s)\n' "$CASES" "$CHECKS" "$FAILS" "$SKIPPED" "$BASH_VERSION"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
