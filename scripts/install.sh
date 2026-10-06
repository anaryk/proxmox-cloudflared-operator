#!/usr/bin/env bash
#
# Installs pco on a Proxmox VE node: downloads the release package, verifies
# it and hands over to the setup. Run it as root.
#
# There are two profiles. The host profile, the default, installs the package
# on the node and hands over to `pco setup`. The appliance profile installs
# nothing on the node: it unpacks the verified package into a temporary
# directory, runs `pco appliance install` from there, which makes a container,
# and removes the directory when that returns. The profile is chosen by
# --appliance or --profile host|appliance among the arguments, else by
# PCO_PROFILE, else, on a terminal, by a question that Enter answers with the
# host profile. --uninstall, with the appliance profile, runs
# `pco appliance uninstall` in the same way. The other arguments are passed on
# to the command the script hands over to.
#
# The package is verified through checksums.txt, and checksums.txt through its
# detached signature by the release key embedded below: a checksum fetched from
# the place the package comes from proves nothing on its own. Every check runs
# before the package is installed or unpacked, and the script stops at the
# first one that fails.
#
# With PCO_INSECURE_SKIP_SIGNATURE=1 only the HTTPS connection to the release
# host (offline: whoever supplied the files) decides what is installed. The
# checksum then only catches a damaged download.
#
# The release is expected to hold pco_<version without the v>_<arch>.deb,
# checksums.txt with lines of the form "<64 hex digits>  <file name>" as
# sha256sum writes them, the detached signature checksums.txt.sig and, for the
# appliance, pco-appliance_<version>_<arch>.tar.zst, which the installer has
# Proxmox download and check against checksums.txt.
#
# Environment:
#   PCO_PROFILE                    host (default) or appliance
#   PCO_VERSION                    release to install, default is the latest
#   PCO_REPO                       GitHub repository, default anaryk/proxmox-cloudflared-operator
#   PCO_SKIP_SETUP=1               verify, and for the host profile install the
#                                  package, then stop and print how to go on
#   PCO_INSECURE_SKIP_SIGNATURE=1  trust the checksum alone, without the signature
#   PCO_DEB, PCO_CHECKSUMS, PCO_SIGNATURE
#                                  local files to install from: the package and
#                                  checksums.txt are not downloaded, but the template
#                                  of the appliance still is, through Proxmox, unless
#                                  PCO_TEMPLATE gives it
#   PCO_TEMPLATE                   appliance: the template as a local file, checked
#                                  against checksums.txt and passed as --template;
#                                  refused with the host profile and with --uninstall
#   PCO_RESUME                     appliance: the journal of an install that did
#                                  not finish, which the installer finishes or takes
#                                  back; refused with the host profile and --uninstall

set -euo pipefail

# Base64 of the binary OpenPGP keyring that holds the release signing key,
# "pco release signing key <tomas.marek@computer-solutions.cz>", fingerprint
# 3D326CB52862A2E91C9919EFA98A1ED57B31F91B. A copy of this script that has the
# placeholder REPLACE-WITH-THE-RELEASE-KEY here instead (a fork that has not set
# its own key) refuses to install unless PCO_INSECURE_SKIP_SIGNATURE=1 is set.
read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true
mDMEasOF+BYJKwYBBAHaRw8BAQdAhXUiIlxzF5qW7lDub4IE4s2vSHPWLsfDNI6N
hj3aena0O3BjbyByZWxlYXNlIHNpZ25pbmcga2V5IDx0b21hcy5tYXJla0Bjb21w
dXRlci1zb2x1dGlvbnMuY3o+iK8EExYKAFcWIQQ9Mmy1KGKi6RyZGe+pih7VezH5
GwUCasOF+BsUgAAAAAAEAA5tYW51MiwyLjUrMS4xMiwwLDMCGwMFCwkIBwICIgIG
FQoJCAsCBBYCAwECHgcCF4AACgkQqYoe1Xsx+RvFtQEAy5H5rDr1lREdBLYMQ33B
m39c8FXBeMaqAsX91wXzjF8A/0xGgRQQrGH9lM1wOrl8elJhj460YhduDkgM5R9q
a98D
EOF

DEFAULT_REPO=anaryk/proxmox-cloudflared-operator
# The setup runs from where the package puts the binary, not from whatever the
# PATH of root holds.
PCO_BIN=/usr/bin/pco
TTY_DEVICE=/dev/tty
# Where `pco appliance install` keeps the journal of a run, outside the
# temporary directory, so that a run that was killed can be finished.
JOURNAL_DIR=/root/.pco-appliance-install
PLACEHOLDER_KEY=REPLACE-WITH-THE-RELEASE-KEY
SIGNATURE_REFUSED="the signature of checksums.txt is not valid for the release key, or the key was revoked or has expired"
SCRIPT_NOTE="install.sh stands for the script as you ran it; when curl piped it into bash, put the variables before that bash"
PROFILE_QUESTION="Install on this node (host profile, the default) or as an appliance (a container, nothing on the node)? [host/appliance]"
MAX_VERSION_LENGTH=64
MAX_SMALL_FILE=1048576
MAX_PACKAGE_FILE=209715200
TMP_DIR=
# What the arguments chose: a profile, and --uninstall. ARGS are the other
# arguments, which are passed on.
CHOSEN=
UNINSTALL=0
ARGS=()
PROFILE=host
# The template file of PCO_TEMPLATE, as an absolute path.
TEMPLATE_FILE=
# Set once the installer of the appliance has been started: from then on a
# failure of the script offers the journal that run may have left.
INSTALLER_STARTED=0
JOURNALS_BEFORE=
RELEASE_VERSION=

say() {
	printf '%s\n' "$*"
}

warn() {
	printf 'install.sh: WARNING: %s\n' "$*" >&2
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [[ -n $TMP_DIR ]]; then
		rm -rf -- "$TMP_DIR" || true
	fi
}

# The variables that made a run what it was, as the words that set them again
# for a command to repeat it with: the appliance profile, which is not the
# default; the release, as the latest one may be another by then, and an
# installer does not go on with the template of another release; and the files
# and settings the run was given. An argument is the journal of a run to finish.
rerun_environment() {
	local journal=${1:-} name pair quoted words=
	local -a pairs=(PCO_PROFILE=appliance)
	if [[ -n $RELEASE_VERSION ]]; then
		pairs+=("PCO_VERSION=$RELEASE_VERSION")
	fi
	for name in PCO_REPO PCO_DEB PCO_CHECKSUMS PCO_SIGNATURE; do
		if [[ -n ${!name:-} ]]; then
			pairs+=("$name=${!name}")
		fi
	done
	if [[ -n $TEMPLATE_FILE ]]; then
		pairs+=("PCO_TEMPLATE=$TEMPLATE_FILE")
	fi
	if [[ ${PCO_INSECURE_SKIP_SIGNATURE:-} == 1 ]]; then
		pairs+=(PCO_INSECURE_SKIP_SIGNATURE=1)
	fi
	if [[ -n $journal ]]; then
		pairs+=("PCO_RESUME=$journal")
	fi
	for pair in "${pairs[@]}"; do
		printf -v quoted '%q' "${pair#*=}"
		words="$words ${pair%%=*}=$quoted"
	done
	printf '%s\n' "${words# }"
}

# The journals of the installer, one path to a line.
journals() {
	local file
	for file in "$JOURNAL_DIR"/*.json; do
		if [[ -e $file ]]; then
			printf '%s\n' "$file"
		fi
	done
}

# A run of the installer that failed, or was killed, keeps its journal outside
# the temporary directory, which is removed first. The journals it names are
# those that were not there before the run, and the one it was told to resume.
offer_resume() {
	local file found=0
	while IFS= read -r file; do
		if [[ -z $file ]]; then
			continue
		fi
		if [[ $'\n'$JOURNALS_BEFORE$'\n' == *$'\n'"$file"$'\n'* && $file != "${PCO_RESUME:-}" ]]; then
			continue
		fi
		if [[ $found == 0 ]]; then
			printf 'install.sh: the installer did not finish, and left its journal in %s\n' "$JOURNAL_DIR" >&2
			found=1
		fi
		printf 'install.sh: to finish the run or take it back, run: %s install.sh\n' "$(rerun_environment "$file")" >&2
	done < <(journals)
	if [[ $found == 1 ]]; then
		printf 'install.sh: %s\n' "$SCRIPT_NOTE" >&2
	fi
}

on_exit() {
	local status=$?
	cleanup
	if [[ $INSTALLER_STARTED == 1 && $status != 0 ]]; then
		offer_resume || true
	fi
}

require_root() {
	local uid
	uid=$(id -u) || die "cannot tell which user runs the script"
	if [[ $uid != 0 ]]; then
		die "run the script as root, it runs as user id $uid"
	fi
}

require_pve() {
	command -v pveversion >/dev/null 2>&1 || die "pveversion not found, this is not a Proxmox VE node"
}

detect_arch() {
	local arch
	command -v dpkg >/dev/null 2>&1 || die "dpkg not found, this is not a Debian based system"
	arch=$(dpkg --print-architecture) || die "dpkg could not tell the architecture"
	case $arch in
	amd64 | arm64) printf '%s\n' "$arch" ;;
	*) die "unsupported architecture $arch, pco is built for amd64 and arm64" ;;
	esac
}

# Proxmox VE 9 is on Debian 13, whose apt depends on sqv and not on gpgv, while
# Proxmox VE 8 has gpgv. Either one will do. The host profile installs the
# package with apt-get, the appliance profile only unpacks it with dpkg-deb.
require_commands() {
	local offline=$1 profile=$2 installer=apt-get cmd missing='' hint=''
	if [[ $profile == appliance ]]; then
		installer=dpkg-deb
	fi
	for cmd in curl sha256sum verifier base64 "$installer" mktemp; do
		if [[ $cmd == curl && $offline == 1 ]]; then
			continue
		fi
		if [[ $cmd == verifier ]]; then
			if ! command -v sqv >/dev/null 2>&1 && ! command -v gpgv >/dev/null 2>&1; then
				missing="$missing, gpgv or sqv"
				hint=" (for the signature check run: apt-get install gpgv)"
			fi
			continue
		fi
		command -v "$cmd" >/dev/null 2>&1 || missing="$missing, $cmd"
	done
	if [[ -n $missing ]]; then
		die "missing required command(s): ${missing#, }$hint"
	fi
}

# Runs before anything is downloaded, so a refusal costs nothing.
check_signature_policy() {
	local skip=$1 offline=$2 who='only the HTTPS connection to the release host decides what is installed'
	local damaged='download'
	if [[ $offline == 1 ]]; then
		who='only whoever supplied the files decides what is installed'
		damaged='file'
	fi
	if [[ $skip == 1 ]]; then
		warn "PCO_INSECURE_SKIP_SIGNATURE=1, the signature of checksums.txt is NOT checked"
		warn "$who; the checksum only catches a damaged $damaged"
		return 0
	fi
	if [[ $PCO_RELEASE_KEY_B64 == "$PLACEHOLDER_KEY" ]]; then
		die "this build of the script carries no release key, so it cannot check the signature; PCO_INSECURE_SKIP_SIGNATURE=1 installs without that check, and then $who"
	fi
}

check_repo() {
	local repo=$1 re='^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$' q
	# The pattern lets "." and ".." through as a whole name; they would walk
	# out of the repository path in the URL.
	if ! [[ $repo =~ $re ]] || [[ $repo == ./* || $repo == ../* || $repo == */. || $repo == */.. ]]; then
		printf -v q '%q' "$repo"
		die "PCO_REPO must look like owner/name, got $q"
	fi
}

check_version() {
	local version=$1 re='^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$' shown q
	if [[ ${#version} -gt $MAX_VERSION_LENGTH ]] || ! [[ $version =~ $re ]]; then
		shown=$version
		if [[ ${#shown} -gt 40 ]]; then
			shown="${shown:0:40}..."
		fi
		printf -v q '%q' "$shown"
		die "refusing the version $q, it must look like 1.2.3 and be at most $MAX_VERSION_LENGTH characters"
	fi
}

check_package_name() {
	local name=$1 re='^[A-Za-z0-9._+~-]+\.deb$' q
	if ! [[ $name =~ $re ]]; then
		printf -v q '%q' "$name"
		die "PCO_DEB must name a .deb file with a plain file name, got $q"
	fi
}

check_local_files() {
	local skip=$1 file
	if [[ -z ${PCO_DEB:-} || -z ${PCO_CHECKSUMS:-} ]]; then
		die "PCO_DEB and PCO_CHECKSUMS must be set together"
	fi
	check_package_name "${PCO_DEB##*/}"
	if [[ $skip != 1 && -z ${PCO_SIGNATURE:-} ]]; then
		die "PCO_SIGNATURE is not set; set it, or set PCO_INSECURE_SKIP_SIGNATURE=1 to go on without a signature"
	fi
	for file in "$PCO_DEB" "$PCO_CHECKSUMS"; do
		if [[ ! -f $file || ! -r $file ]]; then
			die "not a readable file: $file"
		fi
	done
	if [[ $skip != 1 && ( ! -f $PCO_SIGNATURE || ! -r $PCO_SIGNATURE ) ]]; then
		die "not a readable file: $PCO_SIGNATURE"
	fi
}

check_profile_name() {
	local value=$1 what=$2 shown q
	if [[ $value != host && $value != appliance ]]; then
		shown=$value
		if [[ ${#shown} -gt 40 ]]; then
			shown="${shown:0:40}..."
		fi
		printf -v q '%q' "$shown"
		die "$what must be host or appliance, got $q"
	fi
}

choose() {
	check_profile_name "$1" --profile
	if [[ -n $CHOSEN && $CHOSEN != "$1" ]]; then
		die "the arguments name both profiles, host and appliance"
	fi
	CHOSEN=$1
}

# The profile and --uninstall are the script's own, wherever they stand among
# the arguments, and are not passed on: neither pco setup nor pco appliance has
# a flag of those names, so on they could only fail, after the package is
# installed. The other arguments go to ARGS as they are.
own_options() {
	ARGS=()
	while [[ $# -gt 0 ]]; do
		case $1 in
		--appliance)
			choose appliance
			shift
			;;
		--profile)
			if [[ $# -lt 2 ]]; then
				die "--profile needs a value, host or appliance"
			fi
			choose "$2"
			shift 2
			;;
		--profile=*)
			choose "${1#--profile=}"
			shift
			;;
		--uninstall)
			UNINSTALL=1
			shift
			;;
		*)
			ARGS+=("$1")
			shift
			;;
		esac
	done
}

has_yes() {
	local arg
	for arg in "$@"; do
		if [[ $arg == --yes || $arg == -y ]]; then
			return 0
		fi
	done
	return 1
}

# Where the answers to the questions come from: stdin when it is a terminal,
# the terminal when stdin is the script itself (curl | bash), the answers of
# --yes, or nowhere. Opening /dev/tty fails when the process has no controlling
# terminal, even though the file is readable, so it is tried instead of tested.
input_mode() {
	if [[ -t 0 ]]; then
		printf 'stdin\n'
	elif (: <"$TTY_DEVICE") 2>/dev/null; then
		printf 'tty\n'
	elif has_yes "$@"; then
		printf 'yes\n'
	else
		printf 'none\n'
	fi
}

# Asks on the terminal until the answer is one of the two; Enter is the host
# profile. The terminal is opened once, so that an answer is not read twice.
ask_profile() {
	local answer
	if [[ -t 0 ]]; then
		exec 3<&0
	else
		exec 3<"$TTY_DEVICE"
	fi
	while true; do
		printf '%s ' "$PROFILE_QUESTION"
		answer=
		if ! IFS= read -r answer <&3 && [[ -z $answer ]]; then
			die "no answer to the question; PCO_PROFILE=host or PCO_PROFILE=appliance gives it beforehand"
		fi
		answer=${answer//[[:space:]]/}
		case $answer in
		'' | [Hh] | [Hh][Oo][Ss][Tt])
			PROFILE=host
			break
			;;
		[Aa] | [Aa][Pp][Pp][Ll][Ii][Aa][Nn][Cc][Ee])
			PROFILE=appliance
			break
			;;
		esac
		say "answer host or appliance"
	done
	exec 3<&-
}

# The arguments win over PCO_PROFILE, which wins over the question. The
# question is for an install on a terminal: --yes takes the default answers and
# --uninstall has nothing to choose between.
choose_profile() {
	local mode
	if [[ -n ${PCO_PROFILE:-} ]]; then
		check_profile_name "$PCO_PROFILE" PCO_PROFILE
		PROFILE=$PCO_PROFILE
	fi
	if [[ -n $CHOSEN ]]; then
		PROFILE=$CHOSEN
		return 0
	fi
	if [[ -n ${PCO_PROFILE:-} || $UNINSTALL == 1 ]] || has_yes "$@"; then
		return 0
	fi
	mode=$(input_mode)
	if [[ $mode == stdin || $mode == tty ]]; then
		ask_profile
	fi
}

# The installer and the uninstaller ask questions: without a terminal they need
# --yes, and the script says so before it downloads anything.
require_input() {
	if [[ $(input_mode "$@") == none ]]; then
		die "no terminal to ask the installer's questions on: run it from a terminal, or add --yes"
	fi
}

check_resume_file() {
	local file=$1 q
	if [[ $file != /* ]]; then
		printf -v q '%q' "$file"
		die "PCO_RESUME must be the absolute path of a journal, got $q"
	fi
	if [[ ! -f $file || ! -r $file ]]; then
		die "not a readable file: $file"
	fi
}

check_template_file() {
	local file=$1 name=${1##*/} re='^[A-Za-z0-9._+~-]+\.tar\.zst$' q
	if ! [[ $name =~ $re ]]; then
		printf -v q '%q' "$name"
		die "PCO_TEMPLATE must name a .tar.zst file with a plain file name, got $q"
	fi
	if [[ ! -f $file || ! -r $file ]]; then
		die "not a readable file: $file"
	fi
	if [[ $file != /* ]]; then
		file=$PWD/$file
	fi
	TEMPLATE_FILE=$file
}

# What only the appliance profile reads. A variable the chosen profile does not
# read is refused, as it means the profile is not the one that was meant.
check_profile_variables() {
	if [[ $PROFILE == host ]]; then
		if [[ -n ${PCO_TEMPLATE:-} ]]; then
			die "PCO_TEMPLATE is for the appliance profile, which --appliance or PCO_PROFILE=appliance chooses"
		fi
		if [[ -n ${PCO_RESUME:-} ]]; then
			die "PCO_RESUME is for the appliance profile, which --appliance or PCO_PROFILE=appliance chooses"
		fi
		return 0
	fi
	if [[ $UNINSTALL == 1 ]]; then
		if [[ -n ${PCO_RESUME:-} ]]; then
			die "PCO_RESUME finishes an install, it does not go with --uninstall"
		fi
		if [[ -n ${PCO_TEMPLATE:-} ]]; then
			die "PCO_TEMPLATE is for an install, it does not go with --uninstall"
		fi
	fi
	if [[ -n ${PCO_RESUME:-} ]]; then
		check_resume_file "$PCO_RESUME"
	fi
	if [[ -n ${PCO_TEMPLATE:-} ]]; then
		check_template_file "$PCO_TEMPLATE"
	fi
	if [[ ${PCO_SKIP_SETUP:-} != 1 ]]; then
		require_input "$@"
	fi
}

make_tmp() {
	trap on_exit EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
	# TMPDIR is not looked at. The script runs as root, and a TMPDIR that
	# another user owns would let that user rename this directory and put one
	# of theirs in its place between the checks and the install. /tmp is
	# root's and sticky, so only root can do that to a directory in it; the
	# directory itself is private to root.
	TMP_DIR=$(umask 077 && mktemp -d /tmp/pco-install.XXXXXXXX 2>/dev/null) ||
		die "cannot create a temporary directory in /tmp"
}

# --disable has to come first for curl to leave out the curlrc of root, where a
# line such as "insecure" would take the TLS check away.
download() {
	local url=$1 dest=$2 max=$3 detail=''
	# curl's own message goes into the one line of the failure.
	if ! curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
		--connect-timeout 20 --max-time 300 --max-filesize "$max" \
		--output "$dest" "$url" 2>"$TMP_DIR/curl.err"; then
		read -r detail <"$TMP_DIR/curl.err" || true
		die "download failed: $url${detail:+ ($detail)}"
	fi
}

latest_version() {
	local repo=$1 file=$TMP_DIR/latest.json line tag='' re='"tag_name"[[:space:]]*:[[:space:]]*"([^"]*)"'
	download "https://api.github.com/repos/$repo/releases/latest" "$file" "$MAX_SMALL_FILE"
	while IFS= read -r line || [[ -n $line ]]; do
		if [[ $line =~ $re ]]; then
			tag=${BASH_REMATCH[1]}
			break
		fi
	done <"$file"
	if [[ -z $tag ]]; then
		die "the answer of GitHub names no release; set PCO_VERSION to install a given one"
	fi
	printf '%s\n' "$tag"
}

stage_remote() {
	local base=$1 name=$2 skip=$3
	say "downloading $name"
	download "$base/$name" "$TMP_DIR/$name" "$MAX_PACKAGE_FILE"
	say "downloading checksums.txt"
	download "$base/checksums.txt" "$TMP_DIR/checksums.txt" "$MAX_SMALL_FILE"
	if [[ $skip != 1 ]]; then
		say "downloading checksums.txt.sig"
		download "$base/checksums.txt.sig" "$TMP_DIR/checksums.txt.sig" "$MAX_SMALL_FILE"
	fi
}

# The files are copied so that what is verified cannot change before it is installed.
stage_local() {
	local name=$1 skip=$2
	cp -- "$PCO_DEB" "$TMP_DIR/$name" 2>/dev/null || die "cannot copy $PCO_DEB to the temporary directory"
	cp -- "$PCO_CHECKSUMS" "$TMP_DIR/checksums.txt" 2>/dev/null || die "cannot copy $PCO_CHECKSUMS to the temporary directory"
	if [[ $skip != 1 ]]; then
		cp -- "$PCO_SIGNATURE" "$TMP_DIR/checksums.txt.sig" 2>/dev/null || die "cannot copy $PCO_SIGNATURE to the temporary directory"
	fi
}

is_fingerprint() {
	local re='^[0-9A-Fa-f]{40}([0-9A-Fa-f]{24})?$'
	[[ $1 =~ $re ]]
}

# sqv prints the fingerprint of the signing key and exits 0 for a good
# signature by a key of the keyring that is not revoked.
verify_with_sqv() {
	local keyring=$1 sig=$2 file=$3 out line fpr=''
	out=$(sqv --keyring "$keyring" "$sig" "$file" 2>/dev/null) || die "$SIGNATURE_REFUSED"
	while IFS= read -r line; do
		if is_fingerprint "$line"; then
			fpr=$line
			break
		fi
	done <<<"$out"
	if [[ -z $fpr ]]; then
		die "sqv accepted the signature but named no key fingerprint"
	fi
	printf '%s\n' "$fpr"
}

# gpgv exits 0 for a signature by a key that is revoked or has expired and says
# so only in the status lines, so what it reports is read and not just its
# exit code. Given --keyring it does not fall back on the keys in ~/.gnupg.
verify_with_gpgv() {
	local keyring=$1 sig=$2 file=$3 status line fpr='' good=0
	local -a field
	status=$(gpgv --status-fd 1 --keyring "$keyring" "$sig" "$file" 2>/dev/null) || die "$SIGNATURE_REFUSED"
	while IFS= read -r line; do
		case $line in
		'[GNUPG:] BADSIG '* | '[GNUPG:] ERRSIG '* | '[GNUPG:] EXPSIG '* | '[GNUPG:] EXPKEYSIG '* | '[GNUPG:] REVKEYSIG '*)
			die "$SIGNATURE_REFUSED"
			;;
		'[GNUPG:] GOODSIG '*)
			good=1
			;;
		'[GNUPG:] VALIDSIG '*)
			if [[ -z $fpr ]]; then
				read -r -a field <<<"$line"
				# The last field is the fingerprint of the primary key, which differs
				# from the first one when a subkey signed.
				fpr=${field[2]:-}
				if [[ ${#field[@]} -ge 12 ]]; then
					fpr=${field[11]}
				fi
			fi
			;;
		esac
	done <<<"$status"
	if [[ $good != 1 ]]; then
		die "$SIGNATURE_REFUSED"
	fi
	if ! is_fingerprint "$fpr"; then
		die "gpgv accepted the signature but named no key fingerprint"
	fi
	printf '%s\n' "$fpr"
}

# Prints the fingerprint of the key that made the signature.
verify_signature() {
	local keyring=$TMP_DIR/release.gpg sig=$TMP_DIR/checksums.txt.sig file=$TMP_DIR/checksums.txt
	printf '%s\n' "$PCO_RELEASE_KEY_B64" | base64 -d >"$keyring" 2>/dev/null ||
		die "the release key embedded in this script is not valid base64"
	if command -v sqv >/dev/null 2>&1; then
		verify_with_sqv "$keyring" "$sig" "$file"
	else
		verify_with_gpgv "$keyring" "$sig" "$file"
	fi
}

# Prints the checksum of the one line that names the package. The file is read
# line by line instead of being handed to sha256sum -c, which would check
# whatever files it names, by whatever path.
expected_checksum() {
	local sums=$1 name=$2 line found=0 hash='' re='^([0-9a-f]{64})  (.+)$'
	while IFS= read -r line || [[ -n $line ]]; do
		if [[ $line =~ $re ]]; then
			if [[ ${BASH_REMATCH[2]} == "$name" ]]; then
				found=$((found + 1))
				hash=${BASH_REMATCH[1]}
			fi
		fi
	done <"$sums"
	if [[ $found == 0 ]]; then
		die "checksums.txt has no line for $name"
	fi
	if [[ $found != 1 ]]; then
		die "checksums.txt has $found lines for $name, expected one"
	fi
	printf '%s\n' "$hash"
}

verify_checksum() {
	local file=$1 name=$2 expected actual
	expected=$(expected_checksum "$TMP_DIR/checksums.txt" "$name")
	actual=$(sha256sum "$file" 2>/dev/null) || die "sha256sum failed on $name"
	actual=${actual%% *}
	if [[ $actual != "$expected" ]]; then
		die "checksum of $name does not match checksums.txt: expected $expected, got $actual"
	fi
}

# An upgrade keeps the configuration files the admin edited, and takes the new
# ones where nothing was edited: dpkg's question has no one to answer it.
install_package() {
	local deb=$1 name=$2
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$deb" </dev/null ||
		die "apt-get could not install $name"
}

# exec does not run the EXIT trap, so the temporary directory goes first.
exec_setup() {
	cleanup
	trap - EXIT
	exec "$PCO_BIN" setup "$@"
}

hand_over() {
	if [[ ${PCO_SKIP_SETUP:-} == 1 ]]; then
		say "skipping the setup (PCO_SKIP_SETUP=1); run: pco setup"
		return 0
	fi
	if [[ ! -x $PCO_BIN ]]; then
		die "$PCO_BIN is missing or not executable after the install, so the setup cannot start"
	fi
	# Piped into bash, as in curl | bash, stdin is the script and not the
	# terminal. --yes is already among the arguments, which are passed on as
	# they are.
	case $(input_mode "$@") in
	stdin)
		say "starting pco setup"
		exec_setup "$@"
		;;
	tty)
		say "starting pco setup on the terminal"
		exec_setup "$@" <"$TTY_DEVICE"
		;;
	yes)
		say "no terminal, starting pco setup with the answers of --yes"
		exec_setup "$@"
		;;
	*)
		say "no terminal to ask the setup questions on; to finish, run: pco setup"
		;;
	esac
}

# The appliance profile installs nothing: the package is unpacked into the
# temporary directory and pco runs from there.
unpack_package() {
	local deb=$1 name=$2 detail=''
	say "unpacking $name, nothing is installed on this node"
	# The first line of dpkg-deb's own message goes into the one line of the failure.
	if ! dpkg-deb -x "$deb" "$TMP_DIR/root" </dev/null 2>"$TMP_DIR/dpkg-deb.err"; then
		read -r detail <"$TMP_DIR/dpkg-deb.err" || true
		die "dpkg-deb could not unpack $name${detail:+ ($detail)}"
	fi
	if [[ ! -x $TMP_DIR/root/usr/bin/pco ]]; then
		die "$name holds no usr/bin/pco to run"
	fi
}

# Not exec: the temporary directory holds the binary and goes when it returns.
# A signal for the script waits until then, as bash runs its trap only when the
# command it waits for is done; the installer gets the signals of the terminal
# itself and takes back what it made. Its exit code is the exit code of the script.
run_installer() {
	local verb=$1 status=0
	case $(input_mode "$@") in
	stdin)
		say "starting pco appliance $verb"
		"$TMP_DIR/root/usr/bin/pco" appliance "$@" || status=$?
		;;
	tty)
		say "starting pco appliance $verb on the terminal"
		"$TMP_DIR/root/usr/bin/pco" appliance "$@" <"$TTY_DEVICE" || status=$?
		;;
	yes)
		say "no terminal, starting pco appliance $verb with the answers of --yes"
		"$TMP_DIR/root/usr/bin/pco" appliance "$@" || status=$?
		;;
	*)
		die "no terminal to ask the installer's questions on: run it from a terminal, or add --yes"
		;;
	esac
	# Bash's status for a file it cannot run, such as one on a noexec mount; the
	# installer itself exits with 0, 1 or 2.
	if [[ $status == 126 ]]; then
		printf 'install.sh: could not run pco from /tmp (exit status 126), as happens when /tmp is mounted noexec; the script keeps its files in /tmp whatever TMPDIR says, so run it again with /tmp mounted exec: mount -o remount,exec /tmp\n' >&2
	fi
	if [[ $status != 0 ]]; then
		exit "$status"
	fi
}

# The installer is told where the template comes from and what it has to match:
# the release and the checksums.txt that was verified above, which it checks the
# template against again. Offline there is no release to name, and the template
# comes from PCO_TEMPLATE.
hand_over_appliance() {
	local name=$1 base=$2 verb=install again='' quoted
	local -a args
	shift 2
	if [[ $UNINSTALL == 1 ]]; then
		verb=uninstall
	fi
	if [[ ${PCO_SKIP_SETUP:-} == 1 ]]; then
		# pco is not installed and the temporary directory is gone by the time
		# anyone reads this, so what is printed is the script again.
		if [[ $UNINSTALL == 1 ]]; then
			again=--uninstall
		fi
		if [[ $# -gt 0 ]]; then
			printf -v quoted '%q ' "$@"
			again="$again ${quoted% }"
		fi
		say "skipping the appliance $verb (PCO_SKIP_SETUP=1); the package is verified and nothing was installed; to go on, run again without PCO_SKIP_SETUP: $(rerun_environment "${PCO_RESUME:-}") install.sh${again:+ ${again# }}"
		say "$SCRIPT_NOTE"
		return 0
	fi
	if [[ $UNINSTALL == 1 ]]; then
		args=(uninstall "$@")
	else
		args=(install)
		if [[ -n ${PCO_RESUME:-} ]]; then
			args+=(--resume "$PCO_RESUME")
		fi
		if [[ -n $base ]]; then
			args+=(--release-base "$base")
		fi
		args+=(--checksums "$TMP_DIR/checksums.txt")
		if [[ -n $TEMPLATE_FILE ]]; then
			args+=(--template "$TEMPLATE_FILE")
		fi
		args+=("$@")
	fi
	unpack_package "$TMP_DIR/$name" "$name"
	if [[ $verb == install ]]; then
		JOURNALS_BEFORE=$(journals)
		INSTALLER_STARTED=1
	fi
	run_installer "${args[@]}"
}

main() {
	local repo=${PCO_REPO:-$DEFAULT_REPO}
	local arch version name base='' verified fpr offline=0 skip=0

	own_options "$@"
	# ${ARGS[@]+...} keeps an empty ARGS from being an unbound variable in bash 3.2.
	set -- ${ARGS[@]+"${ARGS[@]}"}

	require_root
	require_pve
	arch=$(detect_arch)
	choose_profile "$@"
	if [[ $UNINSTALL == 1 && $PROFILE == host ]]; then
		die "use pco uninstall to take a host install off this node; an appliance is removed with install.sh --appliance --uninstall --vmid <vmid>"
	fi

	if [[ -n ${PCO_DEB:-}${PCO_CHECKSUMS:-}${PCO_SIGNATURE:-} ]]; then
		offline=1
	fi
	if [[ ${PCO_INSECURE_SKIP_SIGNATURE:-} == 1 ]]; then
		skip=1
	fi

	require_commands "$offline" "$PROFILE"
	check_signature_policy "$skip" "$offline"
	check_profile_variables "$@"
	if [[ $offline == 1 ]]; then
		check_local_files "$skip"
	else
		check_repo "$repo"
		if [[ -n ${PCO_VERSION:-} ]]; then
			check_version "$PCO_VERSION"
		fi
	fi

	make_tmp

	if [[ $offline == 1 ]]; then
		name=${PCO_DEB##*/}
		say "package: $name, architecture $arch"
		say "source: local files, nothing is downloaded"
		stage_local "$name" "$skip"
	else
		version=${PCO_VERSION:-}
		if [[ -z $version ]]; then
			say "looking up the latest release of $repo"
			version=$(latest_version "$repo")
			check_version "$version"
		fi
		version=${version#v}
		RELEASE_VERSION=$version
		name=pco_${version}_${arch}.deb
		base=https://github.com/$repo/releases/download/v$version
		say "package: pco $version, architecture $arch"
		say "release: $base/"
		stage_remote "$base" "$name" "$skip"
	fi

	if [[ $skip == 1 ]]; then
		verified="checksum only (signature check skipped)"
	else
		say "checking the signature of checksums.txt"
		fpr=$(verify_signature)
		verified="signature by key $fpr and checksum"
	fi
	say "checking the checksum of $name"
	verify_checksum "$TMP_DIR/$name" "$name"
	if [[ -n $TEMPLATE_FILE ]]; then
		say "checking the checksum of ${TEMPLATE_FILE##*/}"
		verify_checksum "$TEMPLATE_FILE" "${TEMPLATE_FILE##*/}"
	fi

	say "verified by: $verified"
	if [[ $PROFILE == appliance ]]; then
		hand_over_appliance "$name" "$base" "$@"
		return 0
	fi
	say "installing $name"
	install_package "$TMP_DIR/$name" "$name"

	hand_over "$@"
}

{ main "$@"; }
