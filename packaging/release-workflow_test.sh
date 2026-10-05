#!/usr/bin/env bash
#
# Checks the order .github/workflows/release.yml keeps around the signing key.
# The interface is built by the job ui, which holds no secret and may only read
# the repository. The job release needs it, downloads what it built as the
# first step after the checkout, before anything is installed and before the
# key is imported, and runs nothing of npm. The checks run on small workflows
# with each rule broken first, so that they cannot pass by reading nothing.
#
# Run it with: bash packaging/release-workflow_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
WORKFLOW=$HERE/../.github/workflows/release.yml

CHECKS=0
FAILS=0
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/pco-release-workflow-test.XXXXXXXX") || exit 1
trap 'rm -rf "$ROOT"' EXIT

# job <file> <name>: the lines of one job of the workflow.
job() {
	awk -v name="$2" '
		$0 == "  " name ":" { inside = 1; next }
		inside && /^ ? ?[^ #]/ { exit }
		inside { print }
	' "$1"
}

# block <job> <key>: the lines under a key of the job, such as permissions.
block() {
	awk -v key="$2" '
		$0 == "    " key ":" { inside = 1; next }
		inside && /^      / { print; next }
		inside { exit }
	' <<<"$1"
}

# steps <job>: one line per step, its name and the action it uses without the
# version, separated by a bar.
steps() {
	awk '
		function key(s) {
			if (s ~ /^name: /) name[n] = substr(s, 7)
			if (s ~ /^uses: /) { uses[n] = substr(s, 7); sub(/@.*/, "", uses[n]) }
		}
		/^    steps:$/ { inside = 1; next }
		inside && /^      - / { n++; key(substr($0, 9)); next }
		inside && /^        [a-z-]+: / { key(substr($0, 9)) }
		END { for (i = 1; i <= n; i++) printf "%s|%s\n", name[i], uses[i] }
	' <<<"$1"
}

# step <job> <number>: the lines of one step.
step() {
	awk -v want="$2" '
		/^    steps:$/ { inside = 1; next }
		inside && /^      - / { n++ }
		inside && n == want { print }
	' <<<"$1"
}

# check <workflow>: prints each rule the workflow breaks, one line each.
check() {
	local ui release name uses n=0 checkout=0 download=0 tools=0 key=0
	ui=$(job "$1" ui)
	release=$(job "$1" release)

	if [[ -z $ui ]]; then
		echo "there is no job ui"
	else
		if [[ $(block "$ui" permissions) != "      contents: read" ]]; then
			echo "the job ui has permissions other than contents: read of its own"
		fi
		if grep -Eq '^    environment:' <<<"$ui"; then
			echo "the job ui runs in an environment"
		fi
		if grep -q 'secrets\.' <<<"$ui"; then
			echo "the job ui uses a secret"
		fi
		if ! grep -Eq '^      - run: make ui-test ui-budget$' <<<"$ui"; then
			echo "the job ui does not run make ui-test ui-budget"
		fi
		if ! grep -Eq '^          name: ui-dist$' <<<"$ui"; then
			echo "the job ui does not upload ui-dist"
		fi
	fi

	if [[ -z $release ]]; then
		echo "there is no job release"
		return
	fi
	if ! grep -Eq '^    needs: \[?([a-z-]+, )*ui(, [a-z-]+)*\]?$' <<<"$release"; then
		echo "the job release does not need ui"
	fi
	if grep -v '^ *#' <<<"$release" | grep -Eq 'npm|npx|setup-node|make ui'; then
		echo "the job release runs npm"
	fi

	while IFS='|' read -r name uses; do
		n=$((n + 1))
		if [[ $uses == actions/checkout && $checkout == 0 ]]; then
			checkout=$n
		fi
		if [[ $uses == actions/download-artifact && $download == 0 ]]; then
			download=$n
		fi
		if [[ $name == "Install the signature tools" ]]; then
			tools=$n
		fi
		if [[ $name == "Import the release key" ]]; then
			key=$n
		fi
	done < <(steps "$release")

	if [[ $checkout == 0 || $tools == 0 || $key == 0 ]]; then
		echo "the job release lacks the checkout, \"Install the signature tools\" or \"Import the release key\""
		return
	fi
	if [[ $download == 0 ]]; then
		echo "the job release does not download ui-dist"
		return
	fi
	if [[ $download != $((checkout + 1)) ]]; then
		echo "the download of ui-dist is not the first step after the checkout"
	fi
	if ((download > tools || download > key)); then
		echo "the download of ui-dist comes after the signature tools or the key"
	fi
	if ! step "$release" "$download" | grep -Eq '^          name: ui-dist$' ||
		! step "$release" "$download" | grep -Eq '^          path: internal/web/ui/dist$'; then
		echo "the download is not ui-dist into internal/web/ui/dist"
	fi
}

# expect <pass|fail> <case> <file> [a line the problems must contain]
expect() {
	local problems rc
	CHECKS=$((CHECKS + 1))
	problems=$(check "$3")
	rc=$?
	if [[ $rc != 0 ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] the check itself failed, rc %s\n' "$2" "$rc"
	elif [[ $1 == pass && -n $problems ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected no problem, got:\n%s\n' "$2" "$problems"
	elif [[ $1 == fail && $problems != *"${4:-}"* ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected a problem with "%s", got:\n%s\n' "$2" "${4:-}" "${problems:-<none>}"
	elif [[ $1 == fail && -z $problems ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] expected a problem, got none\n' "$2"
	fi
}

# release_step <key>: one step of the release job of a made-up workflow.
release_step() {
	case $1 in
	tag) printf '      - name: Check the tag\n        run: "true"\n' ;;
	checkout) printf '      - uses: actions/checkout@0123 # v4\n' ;;
	download) printf '      - uses: actions/download-artifact@0123 # v8\n        with:\n          name: ui-dist\n          path: internal/web/ui/dist\n' ;;
	elsewhere) printf '      - uses: actions/download-artifact@0123 # v8\n        with:\n          name: ui-dist\n          path: dist\n' ;;
	go) printf '      - uses: actions/setup-go@0123 # v5\n' ;;
	tools) printf '      - name: Install the signature tools\n        run: "true"\n' ;;
	key) printf '      - name: Import the release key\n        run: "true"\n' ;;
	npm) printf '      - run: cd web && npm ci --ignore-scripts\n' ;;
	esac
}

UI_HEAD='    permissions:
      contents: read'
RELEASE_HEAD='    needs: ui'
STEPS='tag checkout download go tools key'

# workflow <file> <ui head> <release head> <release steps>
workflow() {
	local s
	{
		printf 'name: release\n\non:\n  push:\n    tags:\n      - "v*"\n\npermissions:\n  contents: read\n\njobs:\n'
		printf '  ui:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "$2"
		printf '      - uses: actions/checkout@0123 # v4\n'
		printf '      - uses: actions/setup-node@0123 # v7\n        with:\n          node-version-file: web/.nvmrc\n'
		printf '      - run: make ui-test ui-budget\n'
		printf '      - uses: actions/upload-artifact@0123 # v7\n        with:\n          name: ui-dist\n          path: internal/web/ui/dist\n\n'
		printf '  release:\n    runs-on: ubuntu-latest\n    environment: release\n%s\n    steps:\n' "$3"
		for s in $4; do
			release_step "$s"
		done
	} >"$1"
}

f=$ROOT/release.yml

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" "$STEPS"
expect pass 'the order of the release' "$f"

workflow "$f" "$UI_HEAD" '    needs: [lint, ui]' "$STEPS"
expect pass 'needs as a list' "$f"

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" 'tag checkout go tools key download'
expect fail 'the download after the key' "$f" "comes after the signature tools or the key"

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" 'tag checkout go download tools key'
expect fail 'a step between the checkout and the download' "$f" "not the first step after the checkout"

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" 'tag checkout go tools key'
expect fail 'no download' "$f" "does not download ui-dist"

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" 'tag checkout elsewhere go tools key'
expect fail 'a download to another place' "$f" "not ui-dist into internal/web/ui/dist"

workflow "$f" "$UI_HEAD" '' "$STEPS"
expect fail 'the release does not need ui' "$f" "does not need ui"

workflow "$f" "$UI_HEAD" "$RELEASE_HEAD" 'tag checkout download npm go tools key'
expect fail 'npm in the release job' "$f" "runs npm"

workflow "$f" "$UI_HEAD
    environment: release" "$RELEASE_HEAD" "$STEPS"
expect fail 'the ui job in the environment of the release' "$f" "runs in an environment"

workflow "$f" "$UI_HEAD
    env:
      TOKEN: \${{ secrets.TOKEN }}" "$RELEASE_HEAD" "$STEPS"
expect fail 'a secret in the ui job' "$f" "uses a secret"

workflow "$f" '    permissions:
      contents: write' "$RELEASE_HEAD" "$STEPS"
expect fail 'the ui job may write' "$f" "permissions other than contents: read"

workflow "$f" '' "$RELEASE_HEAD" "$STEPS"
expect fail 'the ui job without permissions of its own' "$f" "permissions other than contents: read"

expect pass 'the release workflow of the repository' "$WORKFLOW"

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
