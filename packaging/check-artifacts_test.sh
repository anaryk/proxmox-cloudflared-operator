#!/usr/bin/env bash
#
# Tests for the --require-ui and --require-template checks of
# check-artifacts.sh. Each case lays out a dist directory as goreleaser leaves
# it. For --require-ui its packages carry a program that does nothing, built
# with or without the webui tag; making and reading them takes dpkg-deb and
# go, and with PCO_REQUIRE_DEB_TESTS=1 a missing one fails the run instead of
# skipping those cases. For --require-template a directory beside it holds
# the appliance files that build.sh writes, with templates and packages whose
# programs are compared; those cases take dpkg-deb, zstd and jq, under the
# same rule.
#
# Run it with: bash packaging/check-artifacts_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/check-artifacts.sh
VERSION=0.1.0
AMD64=pco_${VERSION}_amd64.deb
ARM64=pco_${VERSION}_arm64.deb

CASE=
CHECKS=0
FAILS=0
SKIPPED=0
RC=0
ERR=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-check-artifacts-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

# assert <what holds> <command...>
assert() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] %s\n    rc: %s\n    stderr: %s\n' "$CASE" "$what" "$RC" "$ERR"
	fi
}

rc_is() { [[ $RC == "$1" ]]; }
contains() { [[ $1 == *"$2"* ]]; }
lacks() { [[ $1 != *"$2"* ]]; }

# run <arguments...>: leaves RC and ERR.
run() {
	RC=0
	bash "$SUT" "$@" >/dev/null 2>"$ROOT/err" || RC=$?
	ERR=$(<"$ROOT/err")
}

sha256() {
	local out
	if command -v sha256sum >/dev/null 2>&1; then
		out=$(sha256sum "$1")
	else
		out=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${out%% *}"
}

# program <name> [tags]: builds a program that does nothing, with the tags or
# with none, and prints its path.
program() {
	local src=$ROOT/src out=$ROOT/bin/$1 tags=()
	if [[ -n ${2:-} ]]; then
		tags=(-tags "$2")
	fi
	mkdir -p "$src" "$ROOT/bin"
	printf 'module example.invalid/tiny\n\ngo 1.22\n' >"$src/go.mod"
	printf 'package main\n\nfunc main() {}\n' >"$src/main.go"
	(cd "$src" && CGO_ENABLED=0 GOFLAGS='' GOWORK=off go build "${tags[@]}" -o "$out" .) || return 1
	printf '%s\n' "$out"
}

# deb <dir> <arch> [program]: a package of pco with the program as
# ./usr/bin/pco, or without that file when none is given.
deb() {
	local tree=$ROOT/tree-$2
	rm -rf "$tree"
	mkdir -p "$tree/DEBIAN" "$tree/usr/bin"
	printf 'Package: pco\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: test\n' \
		"$VERSION" "$2" >"$tree/DEBIAN/control"
	if [[ -n ${3:-} ]]; then
		cp "$3" "$tree/usr/bin/pco"
	fi
	dpkg-deb --root-owner-group --build "$tree" "$1/pco_${VERSION}_$2.deb" >/dev/null
}

checksums() {
	local name
	for name in "$AMD64" "$ARM64"; do
		printf '%s  %s\n' "$(sha256 "$1/$name")" "$name"
	done >"$1/checksums.txt"
}

# dist <dir> [amd64 program] [arm64 program]: the files of a snapshot of VERSION.
dist() {
	rm -rf "$1"
	mkdir -p "$1"
	printf '{"project_name":"pco","tag":"v%s","version":"%s"}\n' "$VERSION" "$VERSION" >"$1/metadata.json"
	deb "$1" amd64 "${2:-}"
	deb "$1" arm64 "${3:-${2:-}}"
	checksums "$1"
}

CASE='usage'
run --require-ui --bogus
assert "an unknown option beside --require-ui is exit status 2" rc_is 2
assert "and the usage names --require-ui" contains "$ERR" "[--require-ui]"

deb_cases() {
	local missing='' with without bare
	command -v dpkg-deb >/dev/null 2>&1 || missing=dpkg-deb
	command -v go >/dev/null 2>&1 || missing="${missing:+$missing and }go"
	if [[ -n $missing ]]; then
		if [[ ${PCO_REQUIRE_DEB_TESTS:-} == 1 ]]; then
			CASE='the cases with packages'
			RC=1
			ERR=
			assert "PCO_REQUIRE_DEB_TESTS=1 but $missing is not installed" false
		else
			SKIPPED=$((SKIPPED + 1))
			printf 'skipped [the cases with packages]: %s is not installed\n' "$missing"
		fi
		return
	fi
	CASE='building the programs'
	with=$(program with nomsgpack,webui)
	assert "go builds one with the webui tag" test -x "$with"
	without=$(program without nomsgpack)
	assert "one without it" test -x "$without"
	bare=$(program bare)
	assert "and one without any tag" test -x "$bare"
	[[ $FAILS == 0 ]] || return

	CASE='packages built with the webui tag'
	dist "$ROOT/with" "$with"
	run --require-dpkg-deb --require-ui "$ROOT/with"
	assert "pass" rc_is 0

	CASE='packages built without the webui tag'
	dist "$ROOT/without" "$without"
	run --require-dpkg-deb --require-ui "$ROOT/without"
	assert "fail" rc_is 1
	assert "naming the amd64 package" contains "$ERR" "$AMD64 carries a pco built with -tags=nomsgpack;"
	assert "and the arm64 package" contains "$ERR" "$ARM64 carries a pco built with -tags=nomsgpack;"
	assert "with the tags a release needs" contains "$ERR" "needs -tags=nomsgpack,webui"
	run --require-dpkg-deb "$ROOT/without"
	assert "pass when the interface is not asked for" rc_is 0

	CASE='packages built without any tag'
	dist "$ROOT/bare" "$bare"
	run --require-ui "$ROOT/bare"
	assert "fail" rc_is 1
	assert "and say so" contains "$ERR" "$AMD64 carries a pco built without tags;"

	CASE='packages without the program'
	dist "$ROOT/empty"
	run --require-ui "$ROOT/empty"
	assert "fail" rc_is 1
	assert "naming the package" contains "$ERR" "$AMD64 has no ./usr/bin/pco"

	CASE='one package of two built without the tag'
	dist "$ROOT/mixed" "$with" "$without"
	run --require-ui "$ROOT/mixed"
	assert "fails" rc_is 1
	assert "naming that package" contains "$ERR" "$ARM64 carries"
	assert "and not the other" lacks "$ERR" "$AMD64"
}
deb_cases

# The appliance files of a release, which goreleaser lists in checksums.txt
# from PCO_APPLIANCE_DIR, and the cloudflared packages the release keeps
# beside them. Each template is a tar.zst that carries the /usr/bin/pco and
# /usr/bin/cloudflared of the packages. check-artifacts.sh reads the
# cloudflared versions of the repository beside itself, so these cases run a
# copy of it with a manifest of their own. The manifest also allows an older
# cloudflared, of which there is no package: the newest by number, not by
# text, is the one a template carries.
CFV=2026.10.1
PKG=$ROOT/pkg
D=$ROOT/release
A=$ROOT/appliance
TEMPLATES="pco-appliance_${VERSION}_amd64.tar.zst
pco-appliance_${VERSION}_amd64.spdx.json
pco-appliance_${VERSION}_arm64.tar.zst
pco-appliance_${VERSION}_arm64.spdx.json
pco-appliance_${VERSION}.pin.conf
cloudflared-versions.json"

CASE='usage with --require-template'
run --require-template --bogus
assert "an unknown option is exit status 2" rc_is 2
assert "and the usage names --require-template" contains "$ERR" "[--require-template]"

# cloudflared_deb <dir> <arch> <file>: a cloudflared package with the file as
# ./usr/bin/cloudflared.
cloudflared_deb() {
	local tree=$ROOT/tree-cloudflared
	rm -rf "$tree"
	mkdir -p "$tree/DEBIAN" "$tree/usr/bin"
	printf 'Package: cloudflared\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: test\n' \
		"$CFV" "$2" >"$tree/DEBIAN/control"
	cp "$3" "$tree/usr/bin/cloudflared"
	dpkg-deb --root-owner-group --build "$tree" "$1/cloudflared_${CFV}_$2.deb" >/dev/null
}

# template <file> <pco> <cloudflared>: a template with the two files in
# /usr/bin, either left out when its argument is empty.
template() {
	local tree=$ROOT/tree-template
	rm -rf "$tree"
	mkdir -p "$tree/usr/bin" "$tree/etc/pco"
	printf 'appliance\n' >"$tree/etc/pco/profile"
	if [[ -n $2 ]]; then
		cp "$2" "$tree/usr/bin/pco"
	fi
	if [[ -n $3 ]]; then
		cp "$3" "$tree/usr/bin/cloudflared"
	fi
	tar -C "$tree" -cf - . | zstd --quiet --stdout >"$1"
}

# sums <dist> <appliance>: checksums.txt of the packages and the appliance files.
sums() {
	local name
	checksums "$1"
	while IFS= read -r name; do
		printf '%s  %s\n' "$(sha256 "$2/$name")" "$name"
	done <<<"$TEMPLATES" >>"$1/checksums.txt"
}

# release <dist> <appliance>: a release of VERSION with its templates, every
# file listed in checksums.txt.
release() {
	local arch
	rm -rf "$1" "$2"
	mkdir -p "$1" "$2"
	printf '{"project_name":"pco","tag":"v%s","version":"%s"}\n' "$VERSION" "$VERSION" >"$1/metadata.json"
	for arch in amd64 arm64; do
		printf 'pco %s for %s\n' "$VERSION" "$arch" >"$ROOT/pco-$arch"
		printf 'cloudflared %s for %s\n' "$CFV" "$arch" >"$ROOT/cloudflared-$arch"
		deb "$1" "$arch" "$ROOT/pco-$arch"
		cloudflared_deb "$2" "$arch" "$ROOT/cloudflared-$arch"
		template "$2/pco-appliance_${VERSION}_$arch.tar.zst" "$ROOT/pco-$arch" "$ROOT/cloudflared-$arch"
		printf '{"spdxVersion":"SPDX-2.3","name":"%s"}\n' "$arch" >"$2/pco-appliance_${VERSION}_$arch.spdx.json"
		printf '%s  pco_%s_%s.deb\n' "$(sha256 "$1/pco_${VERSION}_$arch.deb")" "$VERSION" "$arch" \
			>"$2/pco-appliance_${VERSION}_$arch.deb.sha256"
	done
	jq --null-input --arg v "$CFV" \
		--arg amd64 "$(sha256 "$2/cloudflared_${CFV}_amd64.deb")" \
		--arg arm64 "$(sha256 "$2/cloudflared_${CFV}_arm64.deb")" '
		def entry($v; $amd64; $arm64): {
			version: $v,
			amd64: {url: "https://github.com/cloudflare/cloudflared/releases/download/\($v)/cloudflared-linux-amd64.deb", sha256: $amd64},
			arm64: {url: "https://github.com/cloudflare/cloudflared/releases/download/\($v)/cloudflared-linux-arm64.deb", sha256: $arm64}
		};
		{schemaVersion: 1, updated: "2026-10-04",
			versions: [entry("2026.9.3"; "0" * 64; "0" * 64), entry($v; $amd64; $arm64)], deny: []}' \
		>"$PKG/cloudflared-versions.json"
	cp "$PKG/cloudflared-versions.json" "$2/cloudflared-versions.json"
	printf 'SNAPSHOT=20261004T000000Z\n' >"$2/pco-appliance_${VERSION}.pin.conf"
	sums "$1" "$2"
}

template_cases() {
	local missing='' tool
	for tool in dpkg-deb zstd jq; do
		command -v "$tool" >/dev/null 2>&1 || missing="${missing:+$missing, }$tool"
	done
	if [[ -n $missing ]]; then
		if [[ ${PCO_REQUIRE_DEB_TESTS:-} == 1 ]]; then
			CASE='the cases with templates'
			RC=1
			ERR=
			assert "PCO_REQUIRE_DEB_TESTS=1 but $missing is not installed" false
		else
			SKIPPED=$((SKIPPED + 1))
			printf 'skipped [the cases with templates]: %s is not installed\n' "$missing"
		fi
		return
	fi
	mkdir -p "$PKG"
	cp "$SUT" "$PKG/check-artifacts.sh"
	SUT=$PKG/check-artifacts.sh

	CASE='a release with its templates'
	release "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "passes" rc_is 0
	run --require-template "$D"
	assert "fails without PCO_APPLIANCE_DIR, as build/appliance holds none" rc_is 1
	assert "naming the directory it looked in" contains "$ERR" "build/appliance has no pco-appliance_${VERSION}_amd64.tar.zst"
	PCO_APPLIANCE_DIR=$A run "$D"
	assert "fails when the templates are not asked for" rc_is 1
	assert "naming a file that checksums.txt should not list" \
		contains "$ERR" "checksums.txt lists pco-appliance_${VERSION}_amd64.tar.zst, which is not a file of this release"

	CASE='a release without the arm64 template'
	release "$D" "$A"
	rm "$A/pco-appliance_${VERSION}_arm64.tar.zst"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming it" contains "$ERR" "$A has no pco-appliance_${VERSION}_arm64.tar.zst"

	CASE='checksums.txt without the snapshot'
	release "$D" "$A"
	grep -v "pin.conf" "$D/checksums.txt" >"$ROOT/sums" && mv "$ROOT/sums" "$D/checksums.txt"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "checksums.txt has 0 lines for pco-appliance_${VERSION}.pin.conf, expected one"

	CASE='an SBOM changed after the checksums'
	release "$D" "$A"
	printf '{}\n' >"$A/pco-appliance_${VERSION}_amd64.spdx.json"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming it" contains "$ERR" "checksums.txt has the wrong checksum for pco-appliance_${VERSION}_amd64.spdx.json"

	CASE='a template built from another package'
	release "$D" "$A"
	printf '%064d  pco_%s_arm64.deb\n' 0 "$VERSION" >"$A/pco-appliance_${VERSION}_arm64.deb.sha256"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming the template" contains "$ERR" "the arm64 template was built from a pco_${VERSION}_arm64.deb with the sha256"
	assert "and not the other" lacks "$ERR" "the amd64 template"

	CASE='a template without the sum of its package'
	release "$D" "$A"
	rm "$A/pco-appliance_${VERSION}_amd64.deb.sha256"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "nothing says which package the amd64 template carries"

	CASE='a sum of another file'
	release "$D" "$A"
	printf '%s  pco_%s_arm64.deb\n' "$(sha256 "$D/$AMD64")" "$VERSION" >"$A/pco-appliance_${VERSION}_amd64.deb.sha256"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says what the line must be" contains "$ERR" "is not one line \"<sha256>  $AMD64\""

	CASE='a snapshot file without a snapshot'
	release "$D" "$A"
	printf 'SNAPSHOT=yesterday\n' >"$A/pco-appliance_${VERSION}.pin.conf"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "pco-appliance_${VERSION}.pin.conf names no snapshot"

	# The sum beside a template is what the job that built it wrote; the
	# program in the template is what it installed.
	CASE='a template with another pco and the sum of the package'
	release "$D" "$A"
	printf 'another pco\n' >"$ROOT/other"
	template "$A/pco-appliance_${VERSION}_amd64.tar.zst" "$ROOT/other" "$ROOT/cloudflared-amd64"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming the template and the package" contains "$ERR" \
		"the amd64 template carries a /usr/bin/pco other than the one in $AMD64"
	assert "and not the other template" lacks "$ERR" "the arm64 template"
	assert "nor its cloudflared" lacks "$ERR" "/usr/bin/cloudflared"

	CASE='a template with another cloudflared'
	release "$D" "$A"
	printf 'another cloudflared\n' >"$ROOT/other"
	template "$A/pco-appliance_${VERSION}_arm64.tar.zst" "$ROOT/pco-arm64" "$ROOT/other"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming the template and the package" contains "$ERR" \
		"the arm64 template carries a /usr/bin/cloudflared other than the one in cloudflared_${CFV}_arm64.deb"
	assert "and not the other template" lacks "$ERR" "the amd64 template"

	CASE='a template without pco'
	release "$D" "$A"
	template "$A/pco-appliance_${VERSION}_amd64.tar.zst" "" "$ROOT/cloudflared-amd64"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "cannot read ./usr/bin/pco out of pco-appliance_${VERSION}_amd64.tar.zst"

	CASE='a template that is not one'
	release "$D" "$A"
	printf 'a template for amd64\n' >"$A/pco-appliance_${VERSION}_amd64.tar.zst"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "cannot read ./usr/bin/pco out of pco-appliance_${VERSION}_amd64.tar.zst"

	# A package that the template matches, but not the one the repository
	# vetted.
	CASE='a cloudflared package that the manifest does not list'
	release "$D" "$A"
	printf 'another cloudflared\n' >"$ROOT/other"
	cloudflared_deb "$A" amd64 "$ROOT/other"
	template "$A/pco-appliance_${VERSION}_amd64.tar.zst" "$ROOT/pco-amd64" "$ROOT/other"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "naming the package" contains "$ERR" "cloudflared_${CFV}_amd64.deb has the sha256 $(sha256 "$A/cloudflared_${CFV}_amd64.deb")"
	assert "and the manifest" contains "$ERR" "$PKG/cloudflared-versions.json lists"

	CASE='no cloudflared package'
	release "$D" "$A"
	rm "$A/cloudflared_${CFV}_arm64.deb"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "$A has no cloudflared_${CFV}_arm64.deb"

	CASE='a manifest other than the one of the repository'
	release "$D" "$A"
	jq '.updated = "2026-10-05"' "$PKG/cloudflared-versions.json" >"$A/cloudflared-versions.json"
	sums "$D" "$A"
	PCO_APPLIANCE_DIR=$A run --require-template "$D"
	assert "fails" rc_is 1
	assert "and says so" contains "$ERR" "$A/cloudflared-versions.json differs from $PKG/cloudflared-versions.json"
}
template_cases

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
