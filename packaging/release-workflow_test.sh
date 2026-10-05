#!/usr/bin/env bash
#
# Checks the order .github/workflows/release.yml keeps around the signing key.
# The interface is built by the job ui and the packages and templates by the
# job build, which hold no secret, run in no environment and may only read
# the repository. The job sign needs both, downloads what ui built as the
# first step after the checkout and the templates right after, tests the
# interface with Go before anything else is installed and before the key is
# imported, and runs nothing of npm and builds no template. Only the job
# publish, in the environment of that name, publishes the release. The checks
# run on small workflows with each rule broken first, so that they cannot
# pass by reading nothing.
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

# job_names <file>: the names of the jobs of the workflow.
job_names() {
	awk '
		/^jobs:$/ { inside = 1; next }
		inside && /^[^ #]/ { exit }
		inside && /^  [a-z][a-z0-9_-]*:$/ { sub(/^  /, ""); sub(/:$/, ""); print }
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

# steps <job>: one line per step, its name, the action it uses without the
# version and the command of a one-line run, separated by bars.
steps() {
	awk '
		function key(s) {
			if (s ~ /^name: /) name[n] = substr(s, 7)
			if (s ~ /^uses: /) { uses[n] = substr(s, 7); sub(/@.*/, "", uses[n]) }
			if (s ~ /^run: [^|>]/) run[n] = substr(s, 6)
		}
		/^    steps:$/ { inside = 1; next }
		inside && /^      - / { n++; key(substr($0, 9)); next }
		inside && /^        [a-z-]+: / { key(substr($0, 9)) }
		END { for (i = 1; i <= n; i++) printf "%s|%s|%s\n", name[i], uses[i], run[i] }
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

# code <job>: the lines of the job that are not comments.
code() {
	grep -v '^ *#' <<<"$1"
}

# needs <job> <name>: whether the job needs the other one.
needs() {
	grep -Eq "^    needs: \[?([a-z-]+, )*$2(, [a-z-]+)*\]?$" <<<"$1"
}

# unprivileged <job> <name>: prints what makes a job that must hold no secret
# more than that.
unprivileged() {
	if [[ $(block "$1" permissions) != "      contents: read" ]]; then
		echo "the job $2 has permissions other than contents: read of its own"
	fi
	if grep -Eq '^    environment:' <<<"$1"; then
		echo "the job $2 runs in an environment"
	fi
	if grep -q 'secrets\.' <<<"$1"; then
		echo "the job $2 uses a secret"
	fi
}

# check <workflow>: prints each rule the workflow breaks, one line each.
check() {
	local ui build sign publish name uses cmd other n=0 checkout=0 download=0 templates=0 tested=0 tools=0 key=0
	ui=$(job "$1" ui)
	build=$(job "$1" build)
	sign=$(job "$1" sign)
	publish=$(job "$1" publish)

	if [[ -z $ui ]]; then
		echo "there is no job ui"
	else
		unprivileged "$ui" ui
		if ! grep -Eq '^      - run: make ui-test ui-budget$' <<<"$ui"; then
			echo "the job ui does not run make ui-test ui-budget"
		fi
		if ! grep -Eq '^          name: ui-dist$' <<<"$ui"; then
			echo "the job ui does not upload ui-dist"
		fi
	fi

	if [[ -z $build ]]; then
		echo "there is no job build"
	else
		unprivileged "$build" build
		if ! needs "$build" ui; then
			echo "the job build does not need ui"
		fi
		if ! grep -q 'packaging/appliance/build.sh' <<<"$build"; then
			echo "the job build builds no template"
		fi
		if ! grep -Eq '^          name: appliance$' <<<"$build"; then
			echo "the job build does not upload appliance"
		fi
	fi

	if [[ -z $publish ]]; then
		echo "there is no job publish"
	else
		if ! grep -Eq '^    environment: publish$' <<<"$publish"; then
			echo "the job publish does not run in the environment publish"
		fi
		if ! needs "$publish" sign; then
			echo "the job publish does not need sign"
		fi
		if grep -q 'secrets\.' <<<"$publish"; then
			echo "the job publish uses a secret"
		fi
	fi
	for other in $(job_names "$1"); do
		[[ $other != publish ]] || continue
		if code "$(job "$1" "$other")" | grep -Eq 'gh release edit|--draft=false'; then
			echo "the job $other publishes the release"
		fi
	done

	if [[ -z $sign ]]; then
		echo "there is no job sign"
		return
	fi
	if ! needs "$sign" ui; then
		echo "the job sign does not need ui"
	fi
	if ! needs "$sign" build; then
		echo "the job sign does not need build"
	fi
	if code "$sign" | grep -Eq 'npm|npx|setup-node|make ui'; then
		echo "the job sign runs npm"
	fi
	if code "$sign" | grep -Eq 'mmdebstrap|qemu|packaging/appliance/build.sh'; then
		echo "the job sign builds templates"
	fi

	while IFS='|' read -r name uses cmd; do
		n=$((n + 1))
		if [[ $uses == actions/checkout && $checkout == 0 ]]; then
			checkout=$n
		fi
		if [[ $uses == actions/download-artifact ]]; then
			if step "$sign" "$n" | grep -Eq '^          name: appliance$'; then
				templates=$n
			elif [[ $download == 0 ]]; then
				download=$n
			fi
		fi
		if [[ $cmd == "go test -tags nomsgpack,webui ./internal/web/ui/" && $tested == 0 ]]; then
			tested=$n
		fi
		if [[ $name == "Install the signature tools" ]]; then
			tools=$n
		fi
		if [[ $name == "Import the release key" ]]; then
			key=$n
		fi
	done < <(steps "$sign")

	if [[ $checkout == 0 || $tools == 0 || $key == 0 ]]; then
		echo "the job sign lacks the checkout, \"Install the signature tools\" or \"Import the release key\""
		return
	fi
	if [[ $download == 0 ]]; then
		echo "the job sign does not download ui-dist"
		return
	fi
	if [[ $download != $((checkout + 1)) ]]; then
		echo "the download of ui-dist is not the first step after the checkout"
	fi
	if ((download > tools || download > key)); then
		echo "the download of ui-dist comes after the signature tools or the key"
	fi
	if ! step "$sign" "$download" | grep -Eq '^          name: ui-dist$' ||
		! step "$sign" "$download" | grep -Eq '^          path: internal/web/ui/dist$'; then
		echo "the download is not ui-dist into internal/web/ui/dist"
	fi
	if [[ $templates == 0 ]]; then
		echo "the job sign does not download the templates"
	elif ((templates != download + 1)); then
		echo "the download of the templates is not the step after the download of ui-dist"
	fi
	if [[ $tested == 0 ]]; then
		echo "the job sign does not test the interface it downloaded"
	elif ((tested < download || tested > tools || tested > key)); then
		echo "the test of the interface is not between the download and the signature tools and the key"
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

# sign_step <key>: one step of the job sign of a made-up workflow.
sign_step() {
	case $1 in
	tag) printf '      - name: Check the tag\n        run: "true"\n' ;;
	checkout) printf '      - uses: actions/checkout@0123 # v4\n' ;;
	download) printf '      - uses: actions/download-artifact@0123 # v8\n        with:\n          name: ui-dist\n          path: internal/web/ui/dist\n' ;;
	elsewhere) printf '      - uses: actions/download-artifact@0123 # v8\n        with:\n          name: ui-dist\n          path: dist\n' ;;
	templates) printf '      - uses: actions/download-artifact@0123 # v8\n        with:\n          name: appliance\n          path: build/appliance\n' ;;
	go) printf '      - uses: actions/setup-go@0123 # v5\n' ;;
	test) printf '      - run: go test -tags nomsgpack,webui ./internal/web/ui/\n' ;;
	untagged) printf '      - run: go test -tags nomsgpack ./internal/web/ui/\n' ;;
	tools) printf '      - name: Install the signature tools\n        run: "true"\n' ;;
	key) printf '      - name: Import the release key\n        run: "true"\n' ;;
	npm) printf '      - run: cd web && npm ci --ignore-scripts\n' ;;
	mmdebstrap) printf '      - run: packaging/appliance/build.sh --arch amd64 --version 0.1.0 --deb x.deb\n' ;;
	publishes) printf '      - run: gh release edit v0.1.0 --draft=false\n' ;;
	esac
}

UI_HEAD='    permissions:
      contents: read'
BUILD_HEAD='    needs: ui
    permissions:
      contents: read'
SIGN_HEAD='    needs: [ui, build]'
PUBLISH_HEAD='    needs: sign
    environment: publish'
STEPS='tag checkout download templates go test tools key'

# workflow <file> <ui head> <sign head> <sign steps> [build head] [publish head]
workflow() {
	local s
	{
		printf 'name: release\n\non:\n  push:\n    tags:\n      - "v*"\n\npermissions:\n  contents: read\n\njobs:\n'
		printf '  ui:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "$2"
		printf '      - uses: actions/checkout@0123 # v4\n'
		printf '      - uses: actions/setup-node@0123 # v7\n        with:\n          node-version-file: web/.nvmrc\n'
		printf '      - run: make ui-test ui-budget\n'
		printf '      - uses: actions/upload-artifact@0123 # v7\n        with:\n          name: ui-dist\n          path: internal/web/ui/dist\n\n'
		printf '  build:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "${5-$BUILD_HEAD}"
		printf '      - run: packaging/appliance/build.sh --arch amd64 --version 0.1.0 --deb x.deb\n'
		printf '      - uses: actions/upload-artifact@0123 # v7\n        with:\n          name: appliance\n          path: build/appliance\n\n'
		printf '  sign:\n    runs-on: ubuntu-latest\n    environment: release\n%s\n    steps:\n' "$3"
		for s in $4; do
			sign_step "$s"
		done
		printf '\n  publish:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "${6-$PUBLISH_HEAD}"
		printf '      - run: gh release edit v0.1.0 --draft=false\n'
	} >"$1"
}

f=$ROOT/release.yml

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect pass 'the order of the release' "$f"

workflow "$f" "$UI_HEAD" '    needs: [lint, ui, build]' "$STEPS"
expect pass 'needs as a longer list' "$f"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout go tools key download templates'
expect fail 'the download after the key' "$f" "comes after the signature tools or the key"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout go download templates tools key'
expect fail 'a step between the checkout and the download' "$f" "not the first step after the checkout"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout templates go tools key'
expect fail 'no download' "$f" "does not download ui-dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout elsewhere templates go tools key'
expect fail 'a download to another place' "$f" "not ui-dist into internal/web/ui/dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go tools key'
expect fail 'no test of the interface' "$f" "does not test the interface it downloaded"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go untagged tools key'
expect fail 'a test without the webui tag' "$f" "does not test the interface it downloaded"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go tools key test'
expect fail 'the test after the key' "$f" "not between the download and the signature tools and the key"

workflow "$f" "$UI_HEAD" '    needs: build' "$STEPS"
expect fail 'sign does not need ui' "$f" "the job sign does not need ui"

workflow "$f" "$UI_HEAD" '    needs: ui' "$STEPS"
expect fail 'sign does not need build' "$f" "the job sign does not need build"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates npm go tools key'
expect fail 'npm in the job sign' "$f" "the job sign runs npm"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key mmdebstrap'
expect fail 'a template built in the job sign' "$f" "the job sign builds templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download go test tools key'
expect fail 'no download of the templates' "$f" "the job sign does not download the templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download go test templates tools key'
expect fail 'the templates downloaded later' "$f" "not the step after the download of ui-dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key publishes'
expect fail 'the job sign publishes' "$f" "the job sign publishes the release"

workflow "$f" "$UI_HEAD
    environment: release" "$SIGN_HEAD" "$STEPS"
expect fail 'the ui job in the environment of the release' "$f" "the job ui runs in an environment"

workflow "$f" "$UI_HEAD
    env:
      TOKEN: \${{ secrets.TOKEN }}" "$SIGN_HEAD" "$STEPS"
expect fail 'a secret in the ui job' "$f" "the job ui uses a secret"

workflow "$f" '    permissions:
      contents: write' "$SIGN_HEAD" "$STEPS"
expect fail 'the ui job may write' "$f" "the job ui has permissions other than contents: read"

workflow "$f" '' "$SIGN_HEAD" "$STEPS"
expect fail 'the ui job without permissions of its own' "$f" "the job ui has permissions other than contents: read"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD
    environment: release"
expect fail 'the build job in the environment of the release' "$f" "the job build runs in an environment"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD
    env:
      KEY: \${{ secrets.PCO_RELEASE_GPG_KEY }}"
expect fail 'a secret in the build job' "$f" "the job build uses a secret"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" '    needs: ui
    permissions:
      contents: write'
expect fail 'the build job may write' "$f" "the job build has permissions other than contents: read"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" '    permissions:
      contents: read'
expect fail 'the build job does not need ui' "$f" "the job build does not need ui"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" '    needs: sign'
expect fail 'publish outside its environment' "$f" "the job publish does not run in the environment publish"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" '    needs: build
    environment: publish'
expect fail 'publish before the signature' "$f" "the job publish does not need sign"

expect pass 'the release workflow of the repository' "$WORKFLOW"

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
