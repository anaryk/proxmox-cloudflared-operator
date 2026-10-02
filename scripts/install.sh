#!/usr/bin/env bash
#
# Installs pco on a Proxmox VE node: downloads the release package, verifies
# it, installs it and hands over to `pco setup`. Run it as root; the arguments
# are passed on to `pco setup`.
#
# The package is verified through checksums.txt, and checksums.txt through its
# detached signature: a checksum fetched from the place the package comes from
# proves nothing on its own. Every check runs before apt-get is called, and the
# script stops at the first one that fails.
#
# Environment:
#   PCO_VERSION                    release to install, default is the latest
#   PCO_REPO                       GitHub repository, default anaryk/proxmox-cloudflared-operator
#   PCO_SKIP_SETUP=1               install the package and stop before `pco setup`
#   PCO_INSECURE_SKIP_SIGNATURE=1  trust the checksum alone, without the signature
#   PCO_DEB, PCO_CHECKSUMS, PCO_SIGNATURE
#                                  local files to install from, nothing is downloaded

set -euo pipefail

# Base64 of the binary OpenPGP keyring that holds the release signing key.
# Until the first release is signed it is a placeholder, and the script then
# refuses to install unless PCO_INSECURE_SKIP_SIGNATURE=1 is set.
read -r -d '' PCO_RELEASE_KEY_B64 <<'EOF' || true
REPLACE-WITH-THE-RELEASE-KEY
EOF

DEFAULT_REPO=anaryk/proxmox-cloudflared-operator
PLACEHOLDER_KEY=REPLACE-WITH-THE-RELEASE-KEY
TMP_DIR=

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

require_commands() {
	local offline=$1 cmd missing=''
	for cmd in curl sha256sum gpgv base64 apt-get mktemp; do
		if [[ $cmd == curl && $offline == 1 ]]; then
			continue
		fi
		command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
	done
	if [[ -n $missing ]]; then
		die "missing required command(s):$missing"
	fi
}

# Runs before anything is downloaded, so a refusal costs nothing.
check_signature_policy() {
	local skip=$1
	if [[ $skip == 1 ]]; then
		warn "PCO_INSECURE_SKIP_SIGNATURE=1, the signature of checksums.txt is NOT checked"
		warn "the package is trusted on its checksum alone, which comes from the same place as the package"
		return 0
	fi
	if [[ $PCO_RELEASE_KEY_B64 == "$PLACEHOLDER_KEY" ]]; then
		die "this build of the script has no release key, so nothing can be verified; set PCO_INSECURE_SKIP_SIGNATURE=1 to go on with the checksum alone"
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
	local version=$1 re='^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$' q
	if ! [[ $version =~ $re ]]; then
		printf -v q '%q' "$version"
		die "refusing the version $q, it must look like 1.2.3"
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

make_tmp() {
	local base=${TMPDIR:-/tmp}
	trap cleanup EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
	# apt-get must be given an absolute path, or it takes the name for a package.
	if [[ $base != /* ]]; then
		base=/tmp
	fi
	# Only root can enter the directory, so what was verified is what gets installed.
	TMP_DIR=$(umask 077 && mktemp -d "$base/pco-install.XXXXXXXX" 2>/dev/null) ||
		die "cannot create a temporary directory in $base"
}

download() {
	local url=$1 dest=$2 detail=''
	# curl's own message goes into the one line of the failure.
	if ! curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
		--output "$dest" "$url" 2>"$TMP_DIR/curl.err"; then
		read -r detail <"$TMP_DIR/curl.err" || true
		die "download failed: $url${detail:+ ($detail)}"
	fi
}

latest_version() {
	local repo=$1 file=$TMP_DIR/latest.json line tag='' re='"tag_name"[[:space:]]*:[[:space:]]*"([^"]*)"'
	download "https://api.github.com/repos/$repo/releases/latest" "$file"
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
	download "$base/$name" "$TMP_DIR/$name"
	say "downloading checksums.txt"
	download "$base/checksums.txt" "$TMP_DIR/checksums.txt"
	if [[ $skip != 1 ]]; then
		say "downloading checksums.txt.sig"
		download "$base/checksums.txt.sig" "$TMP_DIR/checksums.txt.sig"
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

# Prints the fingerprint of the key that made the signature. gpgv would accept
# a signature by any key of the keyring, which holds only the release key.
verify_signature() {
	local keyring=$TMP_DIR/release.gpg status line fpr='' re='^[0-9A-Fa-f]{40}([0-9A-Fa-f]{24})?$'
	local -a field
	printf '%s\n' "$PCO_RELEASE_KEY_B64" | base64 -d >"$keyring" 2>/dev/null ||
		die "the release key embedded in this script is not valid base64"
	status=$(gpgv --status-fd 1 --keyring "$keyring" "$TMP_DIR/checksums.txt.sig" "$TMP_DIR/checksums.txt" 2>/dev/null) ||
		die "the signature of checksums.txt is not valid for the release key"
	while IFS= read -r line; do
		if [[ $line == '[GNUPG:] VALIDSIG '* ]]; then
			read -r -a field <<<"$line"
			# The last field is the fingerprint of the primary key, which differs
			# from the first one when a subkey signed.
			fpr=${field[2]:-}
			if [[ ${#field[@]} -ge 12 ]]; then
				fpr=${field[11]}
			fi
			break
		fi
	done <<<"$status"
	if ! [[ $fpr =~ $re ]]; then
		die "gpgv accepted the signature but named no key fingerprint"
	fi
	printf '%s\n' "$fpr"
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
	local deb=$1 name=$2 expected actual
	expected=$(expected_checksum "$TMP_DIR/checksums.txt" "$name")
	actual=$(sha256sum "$deb" 2>/dev/null) || die "sha256sum failed on $name"
	actual=${actual%% *}
	if [[ $actual != "$expected" ]]; then
		die "checksum of $name does not match checksums.txt: expected $expected, got $actual"
	fi
}

install_package() {
	local deb=$1 name=$2
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$deb" </dev/null ||
		die "apt-get could not install $name"
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

# exec does not run the EXIT trap, so the temporary directory goes first.
exec_setup() {
	cleanup
	trap - EXIT
	exec pco setup "$@"
}

# Opening /dev/tty fails when the process has no controlling terminal, even
# though the file is readable, so it is tried instead of tested.
hand_over() {
	# Overridable so that the tests need no terminal.
	local tty=${PCO_INSTALL_TTY:-/dev/tty}

	if [[ ${PCO_SKIP_SETUP:-} == 1 ]]; then
		say "skipping the setup (PCO_SKIP_SETUP=1); run: pco setup"
		return 0
	fi
	command -v pco >/dev/null 2>&1 || die "pco was installed but is not on the PATH; run: pco setup"
	if [[ -t 0 ]]; then
		say "starting pco setup"
		exec_setup "$@"
	fi
	# Piped into bash, as in curl | bash, stdin is the script and not the terminal.
	if (: <"$tty") 2>/dev/null; then
		say "starting pco setup on the terminal"
		exec_setup "$@" <"$tty"
	fi
	# --yes is already among the arguments, which are passed on as they are.
	if has_yes "$@"; then
		say "no terminal, starting pco setup with the answers of --yes"
		exec_setup "$@"
	fi
	say "no terminal to ask the setup questions on; to finish, run: pco setup"
}

main() {
	local repo=${PCO_REPO:-$DEFAULT_REPO}
	local arch version name base verified fpr offline=0 skip=0

	require_root
	require_pve
	arch=$(detect_arch)

	if [[ -n ${PCO_DEB:-}${PCO_CHECKSUMS:-}${PCO_SIGNATURE:-} ]]; then
		offline=1
	fi
	if [[ ${PCO_INSECURE_SKIP_SIGNATURE:-} == 1 ]]; then
		skip=1
	fi

	require_commands "$offline"
	check_signature_policy "$skip"
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

	say "verified by: $verified"
	say "installing $name"
	install_package "$TMP_DIR/$name" "$name"

	hand_over "$@"
}

main "$@"
