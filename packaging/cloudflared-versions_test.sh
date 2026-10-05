#!/usr/bin/env bash
#
# Checks packaging/cloudflared-versions.json, the cloudflared versions the
# appliance may carry, and tests packaging/cloudflared-update.sh, which adds
# one. The rules run on small manifests that break each of them first, so that
# they cannot pass by reading nothing. The update script runs with a curl of
# this test on the PATH, which serves packages from a directory instead of
# GitHub. With dpkg-deb the packages are real ones and their control files
# are checked as well; with PCO_REQUIRE_DEB_TESTS=1 a missing dpkg-deb fails
# the run instead of skipping those cases.
#
# Run it with: bash packaging/cloudflared-versions_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
MANIFEST=$HERE/cloudflared-versions.json
UPDATE=$HERE/cloudflared-update.sh

CHECKS=0
FAILS=0
SKIPPED=0
CASE=
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-cloudflared-versions-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

# problems <manifest>: one line for each rule the manifest breaks.
problems() {
	jq -r '
		def ver: type == "string" and test("^[0-9]{4}\\.[0-9]{1,2}\\.[0-9]+$");
		def key: split(".") | map(tonumber);
		def month: split(".") | .[0] + "-" + (if (.[1] | length) == 1 then "0" + .[1] else .[1] end) + "-01";
		def date: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")
			and ((try (strptime("%Y-%m-%d") | mktime | strftime("%Y-%m-%d")) catch null) == .);
		(if (.versions | type) == "array" then .versions else [] end) as $versions
		| (if (.deny | type) == "array" then .deny else [] end) as $deny
		| (if .schemaVersion == 1 then empty else "schemaVersion is \(.schemaVersion | tojson), expected 1" end),
		(if .updated | date then empty else "updated is \(.updated | tojson), not a date YYYY-MM-DD" end),
		(if ($versions | length) > 0 then empty else "versions allows no version" end),
		(if (.deny | type) == "array" then empty else "deny is not a list" end),
		($versions[] | . as $e
			| if ($e.version | ver) | not then "the version \($e.version | tojson) is not a version of cloudflared"
			else ("amd64", "arm64") as $a
				| if ($e[$a] | type) != "object" then "\($e.version) has no package for \($a)"
				elif $e[$a].url != "https://github.com/cloudflare/cloudflared/releases/download/\($e.version)/cloudflared-linux-\($a).deb"
				then "\($e.version) names \($e[$a].url | tojson) for \($a), not the package of its release"
				elif ($e[$a].sha256 | type) != "string" or ($e[$a].sha256 | test("^[0-9a-f]{64}$") | not)
				then "\($e.version) has no sha256 for \($a)"
				else empty end
			end),
		($versions | map(.version) | group_by(.)[] | select(length > 1) | "\(.[0]) is allowed \(length) times"),
		($deny[] | if (.version | ver) | not then "the denied version \(.version | tojson) is not a version of cloudflared"
			elif (.reason | type) != "string" or .reason == "" then "the denial of \(.version) gives no reason"
			else empty end),
		($deny | map(.version) | group_by(.)[] | select(length > 1) | "\(.[0]) is denied \(length) times"),
		($versions[] | .version as $v | select(any($deny[]; .version == $v)) | "\($v) is allowed and denied"),
		([$versions[] | .version | select(ver)] | sort_by(key) | last) as $newest
		| if $newest != null and (.updated | date) and .updated < ($newest | month)
		then "updated is \(.updated), before the month of the newest version, \($newest)" else empty end
	' "$1"
}

# expect <pass|fail> <case> <manifest> [a line the problems must contain]
expect() {
	local found rc
	CHECKS=$((CHECKS + 1))
	found=$(problems "$3" 2>&1)
	rc=$?
	if [[ $rc != 0 ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] the check itself failed, rc %s: %s\n' "$2" "$rc" "$found"
	elif [[ $1 == pass && -n $found ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected no problem, got:\n%s\n' "$2" "$found"
	elif [[ $1 == fail && -z $found ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected a problem, got none\n' "$2"
	elif [[ $1 == fail && $found != *"${4:-}"* ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected a problem with "%s", got:\n%s\n' "$2" "${4:-}" "$found"
	fi
}

SHA=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
GOOD='{
  "schemaVersion": 1,
  "updated": "2026-10-04",
  "versions": [
    {
      "version": "2026.9.3",
      "amd64": {"url": "https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-amd64.deb", "sha256": "'$SHA'"},
      "arm64": {"url": "https://github.com/cloudflare/cloudflared/releases/download/2026.9.3/cloudflared-linux-arm64.deb", "sha256": "'$SHA'"}
    }
  ],
  "deny": [{"version": "2026.8.0", "reason": "VULN-141859"}]
}'

# variant <jq filter>: the good manifest changed by the filter, as a file.
variant() {
	local file=$ROOT/variant.json
	jq "$1" <<<"$GOOD" >"$file" || return 1
	printf '%s\n' "$file"
}

printf '%s\n' "$GOOD" >"$ROOT/good.json"
expect pass 'a manifest that keeps every rule' "$ROOT/good.json"
expect fail 'another schema' "$(variant '.schemaVersion = 2')" 'schemaVersion is 2'
expect fail 'no date' "$(variant 'del(.updated)')" 'updated is null'
expect fail 'a date that does not exist' "$(variant '.updated = "2026-02-30"')" 'updated is "2026-02-30"'
expect fail 'a date in another form' "$(variant '.updated = "4.10.2026"')" 'not a date'
expect fail 'no version allowed' "$(variant '.versions = []')" 'versions allows no version'
expect fail 'deny that is not a list' "$(variant '.deny = {}')" 'deny is not a list'
expect fail 'a version that is not one' "$(variant '.versions[0].version = "latest"')" '"latest" is not a version'
expect fail 'one architecture only' "$(variant 'del(.versions[0].arm64)')" '2026.9.3 has no package for arm64'
expect fail 'a package from elsewhere' \
	"$(variant '.versions[0].amd64.url = "https://example.com/cloudflared-linux-amd64.deb"')" 'not the package of its release'
expect fail 'the package of another release' \
	"$(variant '.versions[0].arm64.url = "https://github.com/cloudflare/cloudflared/releases/download/2026.9.2/cloudflared-linux-arm64.deb"')" \
	'not the package of its release'
expect fail 'no sha256' "$(variant 'del(.versions[0].amd64.sha256)')" '2026.9.3 has no sha256 for amd64'
expect fail 'a sha256 that is not one' "$(variant '.versions[0].arm64.sha256 = "ABC"')" 'no sha256 for arm64'
expect fail 'a version allowed twice' "$(variant '.versions += [.versions[0]]')" '2026.9.3 is allowed 2 times'
expect fail 'a version denied twice' "$(variant '.deny += [.deny[0]]')" '2026.8.0 is denied 2 times'
expect fail 'a denial without a reason' "$(variant '.deny[0].reason = ""')" 'the denial of 2026.8.0 gives no reason'
expect fail 'a denied version that is not one' "$(variant '.deny[0].version = "2026.8"')" 'the denied version "2026.8"'
expect fail 'a version allowed and denied' "$(variant '.deny += [{"version": "2026.9.3", "reason": "x"}]')" '2026.9.3 is allowed and denied'
expect fail 'updated before the newest version' "$(variant '.updated = "2026-08-31"')" 'before the month of the newest version, 2026.9.3'
expect pass 'updated on the first day of its month' "$(variant '.updated = "2026-09-01"')"
expect pass 'the newest version by number, not by text' \
	"$(variant '.versions += [.versions[0] | .version = "2026.10.0" | .amd64.url = "https://github.com/cloudflare/cloudflared/releases/download/2026.10.0/cloudflared-linux-amd64.deb" | .arm64.url = "https://github.com/cloudflare/cloudflared/releases/download/2026.10.0/cloudflared-linux-arm64.deb"] | .updated = "2026-10-01"')"
expect pass 'the manifest of the repository' "$MANIFEST"

# The update script, against a curl that serves $SERVE/<version>/<file>.
BIN=$ROOT/bin
SERVE=$ROOT/serve
mkdir -p "$BIN" "$SERVE"
cat >"$BIN/curl" <<'EOF'
#!/usr/bin/env bash
out=
url=
while [[ $# -gt 0 ]]; do
	case $1 in
	--output) out=$2; shift ;;
	https://*) url=$1 ;;
	esac
	shift
done
printf '%s\n' "$url" >>"$CURL_LOG"
prefix=https://github.com/cloudflare/cloudflared/releases/download/
file=$CURL_SERVE/${url#"$prefix"}
if [[ $url != "$prefix"* || ! -f $file ]]; then
	echo "curl: (22) The requested URL returned error: 404" >&2
	exit 22
fi
cp "$file" "$out"
EOF
chmod +x "$BIN/curl"

RC=0
OUT=
ERR=
# update <manifest> <arguments...>: runs the script with the stub; leaves RC, OUT and ERR.
update() {
	RC=0
	: >"$ROOT/curl.log"
	PATH=$BIN:$PATH CURL_SERVE=$SERVE CURL_LOG=$ROOT/curl.log bash "$UPDATE" "${@:2}" "$1" >"$ROOT/out" 2>"$ROOT/err" || RC=$?
	OUT=$(<"$ROOT/out")
	ERR=$(<"$ROOT/err")
}

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
unchanged() { cmp -s "$ROOT/good.json" "$1"; }
no_download() { [[ ! -s $ROOT/curl.log ]]; }
value() { jq -r "$2" "$1"; }

sha256() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	printf '%s\n' "${sum%% *}"
}

HAVE_DEB=1
if ! command -v dpkg-deb >/dev/null 2>&1; then
	HAVE_DEB=0
fi

# package <version> <arch> [package name] [version in the control file]: a
# package to serve, a real one where dpkg-deb is installed.
package() {
	local dir=$SERVE/$1 tree=$ROOT/tree
	mkdir -p "$dir"
	if [[ $HAVE_DEB == 1 ]]; then
		rm -rf "$tree"
		mkdir -p "$tree/DEBIAN"
		printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: test\n' \
			"${3:-cloudflared}" "${4:-$1}" "$2" >"$tree/DEBIAN/control"
		dpkg-deb --root-owner-group --build "$tree" "$dir/cloudflared-linux-$2.deb" >/dev/null
	else
		printf 'not a package: %s %s\n' "$1" "$2" >"$dir/cloudflared-linux-$2.deb"
	fi
}

CASE='a new version'
package 2026.10.0 amd64
package 2026.10.0 arm64
cp "$ROOT/good.json" "$ROOT/m.json"
update "$ROOT/m.json" 2026.10.0
today=$(date -u +%Y-%m-%d)
assert "succeeds" rc_is 0
assert "downloads the amd64 package of the release" contains "$(<"$ROOT/curl.log")" \
	"https://github.com/cloudflare/cloudflared/releases/download/2026.10.0/cloudflared-linux-amd64.deb"
assert "and the arm64 one" contains "$(<"$ROOT/curl.log")" \
	"https://github.com/cloudflare/cloudflared/releases/download/2026.10.0/cloudflared-linux-arm64.deb"
assert "lists it first" [ "$(value "$ROOT/m.json" '.versions[0].version')" == 2026.10.0 ]
assert "with the sha256 of the amd64 package" \
	[ "$(value "$ROOT/m.json" '.versions[0].amd64.sha256')" == "$(sha256 "$SERVE/2026.10.0/cloudflared-linux-amd64.deb")" ]
assert "and of the arm64 package" \
	[ "$(value "$ROOT/m.json" '.versions[0].arm64.sha256')" == "$(sha256 "$SERVE/2026.10.0/cloudflared-linux-arm64.deb")" ]
assert "keeps the version that was there" [ "$(value "$ROOT/m.json" '.versions[1].version')" == 2026.9.3 ]
assert "and the denied ones" [ "$(value "$ROOT/m.json" '.deny | map(.version) | join(" ")')" == 2026.8.0 ]
assert "sets updated to the day" [ "$(value "$ROOT/m.json" '.updated')" == "$today" ]
assert "prints the entry" contains "$OUT" '"version": "2026.10.0"'
assert "and the result keeps every rule" [ -z "$(problems "$ROOT/m.json")" ]

CASE='a version allowed already'
cp "$ROOT/good.json" "$ROOT/m.json"
update "$ROOT/m.json" 2026.9.3
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "allows cloudflared 2026.9.3 already"
assert "without a download" no_download
assert "and changes nothing" unchanged "$ROOT/m.json"

CASE='a denied version'
update "$ROOT/m.json" 2026.8.0
assert "fails" rc_is 1
assert "and says so" contains "$ERR" "denies cloudflared 2026.8.0"
assert "and changes nothing" unchanged "$ROOT/m.json"

CASE='not a version'
update "$ROOT/m.json" '2026.10.0; true'
assert "fails" rc_is 1
assert "and says what a version is" contains "$ERR" "YYYY.M.N"
assert "without a download" no_download
assert "and changes nothing" unchanged "$ROOT/m.json"

CASE='a download that fails'
package 2026.11.0 amd64
update "$ROOT/m.json" 2026.11.0
assert "fails" rc_is 1
assert "naming what it could not download" contains "$ERR" \
	"cannot download https://github.com/cloudflare/cloudflared/releases/download/2026.11.0/cloudflared-linux-arm64.deb"
assert "and changes nothing" unchanged "$ROOT/m.json"

if [[ $HAVE_DEB == 1 ]]; then
	CASE='a package of another version'
	package 2026.12.0 amd64 cloudflared 2026.9.3
	package 2026.12.0 arm64
	update "$ROOT/m.json" 2026.12.0
	assert "fails" rc_is 1
	assert "and says what it is" contains "$ERR" "is not cloudflared 2026.12.0 for amd64 but 'cloudflared 2026.9.3 amd64'"
	assert "and changes nothing" unchanged "$ROOT/m.json"

	CASE='a package of another name'
	package 2027.1.0 amd64
	package 2027.1.0 arm64 cloudflared-fips
	update "$ROOT/m.json" 2027.1.0
	assert "fails" rc_is 1
	assert "and says what it is" contains "$ERR" "but 'cloudflared-fips 2027.1.0 arm64'"
	assert "and changes nothing" unchanged "$ROOT/m.json"
elif [[ ${PCO_REQUIRE_DEB_TESTS:-} == 1 ]]; then
	CASE='the control files'
	RC=1
	ERR=
	assert "PCO_REQUIRE_DEB_TESTS=1 but dpkg-deb is not installed" false
else
	SKIPPED=$((SKIPPED + 1))
	printf 'skipped [the control files]: dpkg-deb is not installed\n'
fi

printf '%d checks, %d failed, %d skipped\n' "$CHECKS" "$FAILS" "$SKIPPED"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
