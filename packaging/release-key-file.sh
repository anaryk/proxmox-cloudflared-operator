#!/usr/bin/env bash
#
# Writes the keyring the package installs as /usr/share/pco/release-key.gpg,
# the keys pco upgrade checks the signature of a release with. goreleaser runs
# it before every build.
#
# A release takes the keys of scripts/install.sh, through release-key.sh, so
# that pco upgrade trusts what the installer of the same commit trusts, and
# fails as release-key.sh fails: on the placeholder of a fork above all.
#
# A snapshot is not a release and gets the test key below, whatever install.sh
# holds. Its private half was thrown away when it was made, so a package of a
# snapshot accepts the signature of no release; a test points it at a keyring
# of its own with PCO_UPGRADE_KEYRING.
#
# Usage: packaging/release-key-file.sh <install.sh> <keyring-file>
#        packaging/release-key-file.sh --snapshot <keyring-file>
#
# Whatever was at <keyring-file> is removed first, so a run that fails leaves
# no keyring of an earlier build to be packaged.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

# "pco snapshot test key, no release is signed with it <snapshot@pco.invalid>",
# fingerprint 62FD5D9D1E944F96940FB17ED1F3BF094E779032.
read -r -d '' TEST_KEY_B64 <<'EOF' || true
mDMEasSWexYJKwYBBAHaRw8BAQdAx4rn+izem+snrjSFf2lYR27j2WfqpAFWjPny
Oe/Nx1m0SnBjbyBzbmFwc2hvdCB0ZXN0IGtleSwgbm8gcmVsZWFzZSBpcyBzaWdu
ZWQgd2l0aCBpdCA8c25hcHNob3RAcGNvLmludmFsaWQ+iK8EExYKAFcWIQRi/V2d
HpRPlpQPsX7R878JTneQMgUCasSWexsUgAAAAAAEAA5tYW51MiwyLjUrMS4xMiww
LDMCGwMFCwkIBwICIgIGFQoJCAsCBBYCAwECHgcCF4AACgkQ0fO/CU53kDLDWwD+
PorsIb534snpW2Knms+Yno49p6OX5jj09LSSpr5nUZkBAPefgHcBXHbPTN7lsqff
QJNvpe2bWsyOAXzYi1+jhg0D
EOF
TEST_KEY_FPR=62FD5D9D1E944F96940FB17ED1F3BF094E779032

die() {
	printf 'release-key-file.sh: %s\n' "$*" >&2
	exit 1
}

usage() {
	printf 'usage: release-key-file.sh <install.sh> <keyring-file>\n       release-key-file.sh --snapshot <keyring-file>\n' >&2
	exit 2
}

if [[ $# != 2 ]]; then
	usage
fi
source=$1
out=$2
if [[ -z $out || $out == --snapshot ]]; then
	usage
fi

rm -f -- "$out" || die "cannot remove $out"
mkdir -p -- "$(dirname -- "$out")" || die "cannot create the directory of $out"

if [[ $source != --snapshot ]]; then
	fprs=$(bash "$HERE/release-key.sh" "$source" "$out") || exit 1
	printf '%s: the release keys of %s: %s\n' "$out" "$source" "${fprs//$'\n'/ }"
	exit 0
fi

tmp=$out.tmp.$$
trap 'rm -f -- "$tmp"' EXIT
printf '%s\n' "$TEST_KEY_B64" | base64 -d >"$tmp" 2>/dev/null || die "the test key is not valid base64"
mv -f -- "$tmp" "$out" || die "cannot write $out"
printf '%s: the test key %s, for a snapshot\n' "$out" "$TEST_KEY_FPR"
