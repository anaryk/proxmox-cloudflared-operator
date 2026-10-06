#!/usr/bin/env bash
#
# Checks the order .github/workflows/release.yml keeps around the signing key.
# The interface is built by the job ui and the packages and templates by the
# job build, which hold no secret, run in no environment and may only read
# the repository. The job build reads the tag after its checkout and builds
# the templates from the snapshot it found. The job sign is the only one in
# an environment, release, whose approval is the one a release waits for. It
# needs both, downloads what ui built as the first step after the checkout
# and the templates right after, tests the interface with Go before anything
# else is installed and before the key is imported, runs nothing of npm,
# builds no template, and deletes its draft when it fails, after the key is
# gone, and never the tag. Nothing of the tag runs while the key is on the
# runner: the before hooks of .goreleaser.yaml, read out of it so that a new
# one cannot be missed, run as steps of their own before the key is
# imported, as goreleaser fills them for a release; its goreleaser skips
# them, while the one of the job build keeps them; and between the import
# and the removal of the key no step runs a program of the repository. Only
# the job publish publishes the release, and it tells whether the release is
# the latest. The documentation site of the latest release is built by the
# job site, which needs publish and runs only for the latest release: it holds
# no secret and may only read the repository, since npm runs in it. The job
# pages deploys what site built, runs only for the latest release too, and is
# the only job besides sign in an environment, github-pages, with no
# permission but pages and id-token and no step but the deployment. The checks
# run on small workflows with each rule broken first, so that they cannot pass
# by reading nothing.
#
# The step that reads the tag also runs, out of the workflow of the
# repository, against tags a release may be pushed with: annotated or not,
# with a line "Snapshot: <timestamp>" in the message or without. The step that
# tells whether the release is the latest runs against the tags of a
# repository with a newer release, a candidate and an older line.
#
# Run it with: bash packaging/release-workflow_test.sh

HERE=$(cd "$(dirname "$0")" && pwd)
WORKFLOW=$HERE/../.github/workflows/release.yml
GORELEASER=$HERE/../.goreleaser.yaml

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

# script <job> <name>: the script of the run of the step of that name, as
# the runner gets it.
script() {
	awk -v want="$2" '
		/^      - / { named = ($0 == "      - name: " want); body = 0; next }
		named && /^        run: \|$/ { body = 1; next }
		body && (/^          / || /^$/) { print substr($0, 11); next }
		body { body = 0 }
	' <<<"$1"
}

# code <job>: the lines of the job that are not comments.
code() {
	grep -v '^ *#' <<<"$1"
}

# hooks <goreleaser config>: its before hooks, one a line, as they are written.
hooks() {
	awk '
		/^before:$/ { before = 1; next }
		before && /^[^ #]/ { exit }
		before && /^  hooks:$/ { inside = 1; next }
		inside && /^    - / { print substr($0, 7) }
	' "$1"
}

# rendered <hook>: the command the job sign runs for a hook, with the templates
# filled as goreleaser fills them for a release; it fails on a template it does
# not know, which the job could not fill either. goreleaser's CommitTimestamp
# is the time of the commit in seconds, which git gives as %ct.
# shellcheck disable=SC2016
rendered() {
	local cmd=$1 snapshot='{{ if .IsSnapshot }}--snapshot{{ else }}scripts/install.sh{{ end }}'
	local stamp='{{ .CommitTimestamp }}' commit='"$(git log -1 --format=%ct)"'
	cmd=${cmd//"$snapshot"/scripts/install.sh}
	cmd=${cmd//"$stamp"/$commit}
	if [[ $cmd == *'{{'* ]]; then
		return 1
	fi
	printf '%s\n' "$cmd"
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
	if grep -q 'secrets\.' <<<"$1"; then
		echo "the job $2 uses a secret"
	fi
}

# environment <job>: the name of the environment the job runs in, written as
# environment: name or with a name: under it.
environment() {
	awk '
		/^    environment: ./ { sub(/^    environment: /, ""); print; exit }
		/^    environment:$/ { inside = 1; next }
		inside && /^      name: / { sub(/^      name: /, ""); print; exit }
		inside && !/^      / { exit }
	' <<<"$1"
}

# latest_only <job>: whether the job runs only when the job publish found the
# release to be the latest.
latest_only() {
	local want="needs[.]publish[.]outputs[.]latest == 'true'"
	grep -Eq "^    if: ([$][{][{] *)?$want( *[}][}])?\$" <<<"$1"
}

# check_site <workflow>: prints each rule the jobs that publish the
# documentation site break, one line each.
check_site() {
	local site pages publish id name uses cmd n=0 decided=0 flagged=0 deploys=0 other=0 built=0 uploaded=0
	site=$(job "$1" site)
	pages=$(job "$1" pages)
	publish=$(job "$1" publish)

	# The decision of the job publish, the one that marks the release as the
	# latest, is what the other two jobs wait for.
	if [[ -n $publish ]]; then
		id=$(block "$publish" outputs | sed -n 's/^      latest: \${{ *steps\.\([a-z-]*\)\.outputs\.latest *}}$/\1/p')
		if [[ -z $id ]]; then
			echo "the job publish has no output latest taken from one of its steps"
		else
			while IFS='|' read -r name uses cmd; do
				n=$((n + 1))
				if step "$publish" "$n" | grep -Eq "^( {8}|      - )id: $id\$" &&
					code "$(step "$publish" "$n")" | grep -q 'packaging/is-latest\.sh'; then
					decided=$n
				fi
				if code "$(step "$publish" "$n")" | grep -Eq 'gh release edit' &&
					step "$publish" "$n" | grep -Eq "steps\\.$id\\.outputs\\.latest"; then
					flagged=$n
				fi
			done < <(steps "$publish")
			if ((decided == 0)); then
				echo "the output latest of the job publish is not decided by packaging/is-latest.sh"
			fi
			if ((flagged == 0)); then
				echo "the job publish does not mark the release as the latest by the step $id"
			fi
		fi
	fi

	if [[ -z $site ]]; then
		echo "there is no job site"
	else
		unprivileged "$site" site
		if ! needs "$site" publish; then
			echo "the job site does not need publish"
		fi
		if ! latest_only "$site"; then
			echo "the job site does not run only for the latest release"
		fi
		while IFS='|' read -r name uses cmd; do
			if [[ $cmd == "make docs docs-lint docs-test" ]]; then
				built=1
			fi
			if [[ $uses == actions/upload-pages-artifact ]]; then
				uploaded=1
			fi
		done < <(steps "$site")
		if ((built == 0)); then
			echo "the job site does not run make docs docs-lint docs-test"
		fi
		if ((uploaded == 0)); then
			echo "the job site does not upload the pages"
		fi
	fi

	if [[ -z $pages ]]; then
		echo "there is no job pages"
		return
	fi
	if ! needs "$pages" site; then
		echo "the job pages does not need site"
	fi
	if ! needs "$pages" publish; then
		echo "the job pages does not need publish"
	fi
	if ! latest_only "$pages"; then
		echo "the job pages does not run only for the latest release"
	fi
	if [[ $(environment "$pages") != github-pages ]]; then
		echo "the job pages does not run in the environment github-pages"
	fi
	if [[ $(block "$pages" permissions | sort) != "      id-token: write
      pages: write" ]]; then
		echo "the job pages has permissions other than pages: write and id-token: write"
	fi
	if grep -q 'secrets\.' <<<"$pages"; then
		echo "the job pages uses a secret"
	fi
	while IFS='|' read -r name uses cmd; do
		if [[ $uses == actions/deploy-pages ]]; then
			deploys=1
		else
			other=1
		fi
	done < <(steps "$pages")
	if ((deploys == 0 || other == 1)); then
		echo "the job pages runs a step other than actions/deploy-pages"
	fi
}

# check <workflow> <goreleaser config>: prints each rule the workflow breaks,
# one line each.
check() {
	local ui build sign publish name uses cmd other n=0 checkout=0 download=0 templates=0 tested=0 tools=0 key=0 forget=0 deletes=0
	local tagread=0 built=0 release=0 hook want at
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
		checkout=0
		while IFS='|' read -r name uses cmd; do
			n=$((n + 1))
			if [[ $uses == actions/checkout && $checkout == 0 ]]; then
				checkout=$n
			fi
			if [[ $name == "Read the tag" ]]; then
				tagread=$n
			fi
			if step "$build" "$n" | grep -q 'packaging/appliance/build.sh' && [[ $built == 0 ]]; then
				built=$n
			fi
			# This job holds no key: goreleaser runs its hooks itself.
			if [[ $uses == goreleaser/goreleaser-action ]] && step "$build" "$n" | grep -Eq -- '--skip=([a-z]+,)*before'; then
				echo "the job build skips the before hooks of goreleaser"
			fi
		done < <(steps "$build")
		if ((tagread == 0 || checkout == 0 || tagread < checkout || (built != 0 && tagread > built))); then
			echo "the job build does not read the tag between its checkout and the templates"
		elif ! step "$build" "$tagread" | grep -Eq '^        id: tag$'; then
			echo "the step Read the tag has no id tag"
		fi
		if ((built != 0)) && { ! step "$build" "$built" | grep -q 'steps\.tag\.outputs\.snapshot' ||
			step "$build" "$built" | grep -q 'inputs\.snapshot'; }; then
			echo "the job build builds the templates from another snapshot than the one it read"
		fi
	fi

	# One approval per release: the one of the environment of the key. The job
	# pages deploys to the environment of the site, which holds no key and
	# waits for no one; check_site looks at it.
	for other in $(job_names "$1"); do
		[[ $other != sign && $other != pages ]] || continue
		if grep -Eq '^    environment:' <<<"$(job "$1" "$other")"; then
			echo "the job $other runs in an environment"
		fi
	done

	if [[ -z $publish ]]; then
		echo "there is no job publish"
	else
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

	check_site "$1"

	if [[ -z $sign ]]; then
		echo "there is no job sign"
		return
	fi
	if ! grep -Eq '^    environment: release$' <<<"$sign"; then
		echo "the job sign does not run in the environment release"
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
	if code "$sign" | grep -q -- '--cleanup-tag'; then
		echo "the job sign deletes the tag with the draft"
	fi

	n=0
	checkout=0
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
		if [[ $name == "Remove the private key" ]]; then
			forget=$n
		fi
		if [[ $uses == goreleaser/goreleaser-action && $release == 0 ]]; then
			release=$n
		fi
		if code "$(step "$sign" "$n")" | grep -q 'gh release delete'; then
			deletes=$n
		fi
	done < <(steps "$sign")

	if [[ $checkout == 0 || $tools == 0 || $key == 0 || $forget == 0 ]]; then
		echo "the job sign lacks the checkout, \"Install the signature tools\", \"Import the release key\" or \"Remove the private key\""
		return
	fi

	# Nothing of the tag runs while the key is on the runner.
	if [[ $release == 0 ]]; then
		echo "the job sign runs no goreleaser"
	elif ! step "$sign" "$release" | grep -Eq -- '^          args: .*--skip=([a-z]+,)*before(,[a-z]+)*( |$)'; then
		echo "the goreleaser of the job sign runs the before hooks, while the key is on the runner"
	fi
	while IFS= read -r hook; do
		if ! want=$(rendered "$hook"); then
			echo "the before hook \"$hook\" has a template the job sign cannot fill"
			continue
		fi
		at=0
		n=0
		while IFS='|' read -r name uses cmd; do
			n=$((n + 1))
			if [[ $cmd == "$want" && $at == 0 ]]; then
				at=$n
			fi
		done < <(steps "$sign")
		if ((at == 0)); then
			echo "the job sign does not run the before hook: $want"
		elif ((at > key)); then
			echo "the job sign runs the before hook after the key is imported: $want"
		fi
	done < <(hooks "$2")
	for ((n = key + 1; n < forget; n++)); do
		if code "$(step "$sign" "$n")" | grep -Eq '(^|[^A-Za-z0-9_.-])(\./|packaging/|scripts/|hack/|cmd/|internal/)|go (run|test|build|generate)|make '; then
			echo "the job sign runs a program of the repository while the key is on the runner"
		fi
	done
	if [[ $deletes == 0 ]]; then
		echo "the job sign leaves the draft of a failed run"
	else
		if ! step "$sign" "$deletes" | grep -Eq '^        if: (\$\{\{ *)?failure\(\)( *\}\})?$'; then
			echo "the job sign deletes its draft on something else than a failure"
		fi
		if ((deletes < forget)); then
			echo "the job sign deletes its draft before the private key is removed"
		fi
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

# expect <pass|fail> <case> <file> [a line the problems must contain]: the
# workflow checked against the goreleaser config CONFIG names, the one of the
# repository unless set.
CONFIG=
expect() {
	local problems rc
	CHECKS=$((CHECKS + 1))
	problems=$(check "$3" "${CONFIG:-$GORELEASER}")
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

# sign_step <key>: one step of the job sign of a made-up workflow. What looks
# like an expansion in it is for the runner.
# shellcheck disable=SC2016
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
	hooks)
		while IFS= read -r hook; do
			printf '      - run: %s\n' "$(rendered "$hook")"
		done < <(hooks "$GORELEASER")
		;;
	program) printf '      - name: Check the key against install.sh\n        run: packaging/release-key.sh scripts/install.sh "$RUNNER_TEMP/release.gpg"\n' ;;
	key) printf '      - name: Import the release key\n        run: "true"\n' ;;
	goreleaser) printf '      - uses: goreleaser/goreleaser-action@0123 # v7\n        with:\n          args: release --clean --skip=before\n' ;;
	goreleaser_hooks) printf '      - uses: goreleaser/goreleaser-action@0123 # v7\n        with:\n          args: release --clean\n' ;;
	forget) printf '      - name: Remove the private key\n        if: always()\n        run: rm -rf "$RUNNER_TEMP/gnupg"\n' ;;
	files) printf '      - name: Check the release files\n        run: "true"\n' ;;
	delete) printf '      - name: Delete the draft\n        if: failure()\n        run: |\n          gh release delete "$TAG" --yes\n' ;;
	delete_always) printf '      - name: Delete the draft\n        if: always()\n        run: |\n          gh release delete "$TAG" --yes\n' ;;
	delete_tag) printf '      - name: Delete the draft\n        if: failure()\n        run: |\n          gh release delete "$TAG" --yes --cleanup-tag\n' ;;
	npm) printf '      - run: cd web && npm ci --ignore-scripts\n' ;;
	mmdebstrap) printf '      - run: packaging/appliance/build.sh --arch amd64 --version 0.1.0 --deb x.deb\n' ;;
	publishes) printf '      - run: gh release edit v0.1.0 --draft=false\n' ;;
	esac
}

# build_step <key>: one step of the job build of a made-up workflow.
# shellcheck disable=SC2016
build_step() {
	case $1 in
	tag) printf '      - name: Check the tag\n        run: "true"\n' ;;
	checkout) printf '      - uses: actions/checkout@0123 # v4\n' ;;
	read) printf '      - name: Read the tag\n        id: tag\n        run: "true"\n' ;;
	read_noid) printf '      - name: Read the tag\n        run: "true"\n' ;;
	templates) printf '      - name: Build the templates\n        env:\n          SNAPSHOT: ${{ steps.tag.outputs.snapshot }}\n        run: packaging/appliance/build.sh --arch amd64 --version 0.1.0 --deb x.deb\n' ;;
	input) printf '      - name: Build the templates\n        env:\n          SNAPSHOT: ${{ inputs.snapshot }}\n        run: packaging/appliance/build.sh --arch amd64 --version 0.1.0 --deb x.deb\n' ;;
	upload) printf '      - uses: actions/upload-artifact@0123 # v7\n        with:\n          name: appliance\n          path: build/appliance\n' ;;
	goreleaser) printf '      - uses: goreleaser/goreleaser-action@0123 # v7\n        with:\n          args: release --clean --skip=publish,sign,announce\n' ;;
	goreleaser_skip) printf '      - uses: goreleaser/goreleaser-action@0123 # v7\n        with:\n          args: release --clean --skip=publish,sign,before,announce\n' ;;
	esac
}

# publish_step <key>: one step of the job publish of a made-up workflow.
# shellcheck disable=SC2016
publish_step() {
	case $1 in
	latest) printf '      - name: Check whether this is the latest release\n        id: latest\n        run: packaging/is-latest.sh "$TAG"\n' ;;
	undecided) printf '      - name: Check whether this is the latest release\n        id: latest\n        run: "true"\n' ;;
	publish) printf '      - name: Publish the release\n        env:\n          LATEST: ${{ steps.latest.outputs.latest }}\n        run: gh release edit v0.1.0 --draft=false\n' ;;
	blind) printf '      - name: Publish the release\n        run: gh release edit v0.1.0 --draft=false\n' ;;
	esac
}

# site_step <key>: one step of the job site of a made-up workflow.
site_step() {
	case $1 in
	checkout) printf '      - uses: actions/checkout@0123 # v4\n' ;;
	node) printf '      - uses: actions/setup-node@0123 # v7\n        with:\n          node-version-file: web/.nvmrc\n' ;;
	make) printf '      - run: make docs docs-lint docs-test\n' ;;
	upload) printf '      - uses: actions/upload-pages-artifact@0123 # v5\n        with:\n          path: site/.vitepress/dist\n' ;;
	esac
}

# pages_step <key>: one step of the job pages of a made-up workflow.
pages_step() {
	case $1 in
	deploy) printf '      - id: deploy\n        uses: actions/deploy-pages@0123 # v5\n' ;;
	checkout) printf '      - uses: actions/checkout@0123 # v4\n' ;;
	esac
}

UI_HEAD='    permissions:
      contents: read'
BUILD_HEAD='    needs: ui
    permissions:
      contents: read'
RELEASE_ENV='    environment: release'
SIGN_HEAD="    needs: [ui, build]
$RELEASE_ENV"
# shellcheck disable=SC2016
PUBLISH_OUTPUT='    outputs:
      latest: ${{ steps.latest.outputs.latest }}'
PUBLISH_HEAD="    needs: sign
$PUBLISH_OUTPUT"
LATEST_ONLY="    if: needs.publish.outputs.latest == 'true'"
SITE_HEAD="    needs: publish
$LATEST_ONLY
    permissions:
      contents: read"
# shellcheck disable=SC2016
PAGES_ENV='    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}'
PAGES_HEAD="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write
$PAGES_ENV"
STEPS='tag checkout download templates go test tools hooks key goreleaser forget files delete'
BUILD_STEPS='tag checkout read goreleaser templates upload'

# workflow <file> <ui head> <sign head> <sign steps> [build head] [publish head] [build steps]
#
# The jobs after sign are as they should be, unless the call sets SITE or
# PAGES to the head of the job, PUBLISH_STEPS, SITE_STEPS or PAGES_STEPS to
# the keys of its steps, or NO_SITE or NO_PAGES to drop it.
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
		for s in ${7-$BUILD_STEPS}; do
			build_step "$s"
		done
		printf '\n  sign:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "$3"
		for s in $4; do
			sign_step "$s"
		done
		printf '\n  publish:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "${6-$PUBLISH_HEAD}"
		for s in ${PUBLISH_STEPS-latest publish}; do
			publish_step "$s"
		done
		if [[ -z ${NO_SITE:-} ]]; then
			printf '\n  site:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "${SITE-$SITE_HEAD}"
			for s in ${SITE_STEPS-checkout node make upload}; do
				site_step "$s"
			done
		fi
		if [[ -z ${NO_PAGES:-} ]]; then
			printf '\n  pages:\n    runs-on: ubuntu-latest\n%s\n    steps:\n' "${PAGES-$PAGES_HEAD}"
			for s in ${PAGES_STEPS-deploy}; do
				pages_step "$s"
			done
		fi
	} >"$1"
}

f=$ROOT/release.yml

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect pass 'the order of the release' "$f"

workflow "$f" "$UI_HEAD" "    needs: [lint, ui, build]
$RELEASE_ENV" "$STEPS"
expect pass 'needs as a longer list' "$f"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout go tools key download templates goreleaser forget files delete'
expect fail 'the download after the key' "$f" "comes after the signature tools or the key"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout go download templates tools key goreleaser forget files delete'
expect fail 'a step between the checkout and the download' "$f" "not the first step after the checkout"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout templates go tools key goreleaser forget files delete'
expect fail 'no download' "$f" "does not download ui-dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout elsewhere templates go tools key goreleaser forget files delete'
expect fail 'a download to another place' "$f" "not ui-dist into internal/web/ui/dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go tools key goreleaser forget files delete'
expect fail 'no test of the interface' "$f" "does not test the interface it downloaded"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go untagged tools key goreleaser forget files delete'
expect fail 'a test without the webui tag' "$f" "does not test the interface it downloaded"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go tools key test goreleaser forget files delete'
expect fail 'the test after the key' "$f" "not between the download and the signature tools and the key"

workflow "$f" "$UI_HEAD" "    needs: build
$RELEASE_ENV" "$STEPS"
expect fail 'sign does not need ui' "$f" "the job sign does not need ui"

workflow "$f" "$UI_HEAD" "    needs: ui
$RELEASE_ENV" "$STEPS"
expect fail 'sign does not need build' "$f" "the job sign does not need build"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates npm go test tools key goreleaser forget files delete'
expect fail 'npm in the job sign' "$f" "the job sign runs npm"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key mmdebstrap goreleaser forget files delete'
expect fail 'a template built in the job sign' "$f" "the job sign builds templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download go test tools key goreleaser forget files delete'
expect fail 'no download of the templates' "$f" "the job sign does not download the templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download go test templates tools key goreleaser forget files delete'
expect fail 'the templates downloaded later' "$f" "not the step after the download of ui-dist"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS publishes"
expect fail 'the job sign publishes' "$f" "the job sign publishes the release"

workflow "$f" "$UI_HEAD" '    needs: [ui, build]' "$STEPS"
expect fail 'sign outside the environment of the key' "$f" "the job sign does not run in the environment release"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser forget files'
expect fail 'a failed run that leaves its draft' "$f" "the job sign leaves the draft of a failed run"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser forget files delete_always'
expect fail 'a draft deleted whatever the outcome' "$f" "the job sign deletes its draft on something else than a failure"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser delete forget files'
expect fail 'a draft deleted while the key is there' "$f" "the job sign deletes its draft before the private key is removed"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser forget files delete_tag'
expect fail 'the tag deleted with the draft' "$f" "the job sign deletes the tag with the draft"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser files delete'
expect fail 'the private key left on the runner' "$f" "lacks the checkout"

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

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag checkout templates upload'
expect fail 'the build job does not read the tag' "$f" "the job build does not read the tag between its checkout and the templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag read checkout templates upload'
expect fail 'the tag read before the checkout' "$f" "the job build does not read the tag between its checkout and the templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag checkout templates read upload'
expect fail 'the tag read after the templates' "$f" "the job build does not read the tag between its checkout and the templates"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag checkout read_noid templates upload'
expect fail 'the step that reads the tag without its id' "$f" "the step Read the tag has no id tag"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag checkout read input upload'
expect fail 'the templates from the input only' "$f" "the job build builds the templates from another snapshot than the one it read"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD
    environment: publish"
expect fail 'a second approval before publishing' "$f" "the job publish runs in an environment"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "    needs: build
$PUBLISH_OUTPUT"
expect fail 'publish before the signature' "$f" "the job publish does not need sign"

# The documentation site of the latest release: the job publish says whether
# the release is the latest, site builds it with no more than read access and
# pages deploys it, each only for the latest release.
workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD
    environment: github-pages"
expect fail 'publish in the environment of the site' "$f" "the job publish runs in an environment"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD
    environment: github-pages"
expect fail 'the build job in the environment of the site' "$f" "the job build runs in an environment"

PAGES='    needs: [publish, site]
'"$LATEST_ONLY"'
    permissions:
      pages: write
      id-token: write
    environment: github-pages' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect pass 'the environment of the site on one line' "$f"

SITE="$SITE_HEAD
    environment: github-pages" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job in the environment of the site' "$f" "the job site runs in an environment"

SITE="$SITE_HEAD
    environment: release" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job in the environment of the release' "$f" "the job site runs in an environment"

SITE="$SITE_HEAD
    env:
      KEY: \${{ secrets.PCO_RELEASE_GPG_KEY }}" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a secret in the site job' "$f" "the job site uses a secret"

SITE="    needs: publish
$LATEST_ONLY
    permissions:
      contents: write" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job may write' "$f" "the job site has permissions other than contents: read"

SITE="    needs: publish
$LATEST_ONLY
    permissions:
      contents: read
      pages: write" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job may write the pages' "$f" "the job site has permissions other than contents: read"

SITE="    needs: publish
$LATEST_ONLY" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job without permissions of its own' "$f" "the job site has permissions other than contents: read"

SITE='    needs: publish
    permissions:
      contents: read' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job for every release' "$f" "the job site does not run only for the latest release"

SITE="    needs: publish
    if: needs.publish.outputs.latest != 'true'
    permissions:
      contents: read" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job for a release that is not the latest' "$f" "the job site does not run only for the latest release"

SITE="    needs: sign
$LATEST_ONLY
    permissions:
      contents: read" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'the site job that does not wait for publish' "$f" "the job site does not need publish"

SITE_STEPS='checkout node upload' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a site that is not built' "$f" "the job site does not run make docs docs-lint docs-test"

SITE_STEPS='checkout node make' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a site that is not uploaded' "$f" "the job site does not upload the pages"

NO_SITE=1 workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'no site job' "$f" "there is no job site"

NO_PAGES=1 workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'no pages job' "$f" "there is no job pages"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages outside the environment of the site' "$f" "the job pages does not run in the environment github-pages"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write
    environment: release" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages in the environment of the release' "$f" "the job pages does not run in the environment github-pages"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write
    environment: github-pages-preview" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages in an environment that only starts like the one of the site' "$f" "the job pages does not run in the environment github-pages"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      contents: write
      pages: write
      id-token: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that may write the repository' "$f" "the job pages has permissions other than pages: write and id-token: write"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      pages: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages without the token to prove itself' "$f" "the job pages has permissions other than pages: write and id-token: write"

PAGES="    needs: [publish, site]
$LATEST_ONLY
    permissions:
      id-token: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that may not write the pages' "$f" "the job pages has permissions other than pages: write and id-token: write"

PAGES="    needs: [publish, site]
$LATEST_ONLY
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages with the permissions of the workflow' "$f" "the job pages has permissions other than pages: write and id-token: write"

PAGES="$PAGES_HEAD
    env:
      KEY: \${{ secrets.PCO_RELEASE_GPG_KEY }}" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a secret in the pages job' "$f" "the job pages uses a secret"

PAGES="    needs: [publish, site]
    permissions:
      pages: write
      id-token: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages for every release' "$f" "the job pages does not run only for the latest release"

PAGES="    needs: publish
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that does not wait for the site' "$f" "the job pages does not need site"

PAGES="    needs: site
$LATEST_ONLY
    permissions:
      pages: write
      id-token: write
$PAGES_ENV" workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that does not wait for publish' "$f" "the job pages does not need publish"

PAGES_STEPS='checkout deploy' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that runs a step of the repository' "$f" "the job pages runs a step other than actions/deploy-pages"

PAGES_STEPS='checkout' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'pages that deploys nothing' "$f" "the job pages runs a step other than actions/deploy-pages"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" '    needs: sign'
expect fail 'publish that gives no output' "$f" "the job publish has no output latest taken from one of its steps"

PUBLISH_STEPS='undecided publish' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'an output that is not decided by is-latest.sh' "$f" "the output latest of the job publish is not decided by packaging/is-latest.sh"

PUBLISH_STEPS='publish' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'an output taken from a step that is not there' "$f" "the output latest of the job publish is not decided by packaging/is-latest.sh"

PUBLISH_STEPS='latest blind' workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a release marked by another decision than the one the site waits for' "$f" "the job publish does not mark the release as the latest by the step latest"

CHECKS=$((CHECKS + 1))
if [[ $(hooks "$GORELEASER" | grep -c .) -lt 2 ]]; then
	FAILS=$((FAILS + 1))
	printf 'FAIL [the before hooks] fewer than two read out of %s\n' "$GORELEASER"
fi

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key goreleaser forget files delete'
expect fail 'no before hook in the job sign' "$f" "the job sign does not run the before hook: go run -tags nomsgpack ./cmd/pco docs completion build/completion"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools key hooks goreleaser forget files delete'
expect fail 'the before hooks after the key' "$f" "the job sign runs the before hook after the key is imported: test -f internal/web/ui/dist/index.html"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools hooks key goreleaser_hooks forget files delete'
expect fail 'goreleaser runs its hooks in the job sign' "$f" "the goreleaser of the job sign runs the before hooks"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools hooks key forget files delete'
expect fail 'no goreleaser in the job sign' "$f" "the job sign runs no goreleaser"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" 'tag checkout download templates go test tools hooks key program goreleaser forget files delete'
expect fail 'a script of the repository while the key is there' "$f" "runs a program of the repository while the key is on the runner"

workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS" "$BUILD_HEAD" "$PUBLISH_HEAD" 'tag checkout read goreleaser_skip templates upload'
expect fail 'the job build skips the hooks' "$f" "the job build skips the before hooks of goreleaser"

# A hook added to .goreleaser.yaml is found missing in the job sign.
CONFIG=$ROOT/goreleaser.yaml
{
	printf 'before:\n  hooks:\n'
	hooks "$GORELEASER" | sed 's/^/    - /'
	printf '    - make man\n'
	printf 'builds:\n  - id: pco\n'
} >"$CONFIG"
workflow "$f" "$UI_HEAD" "$SIGN_HEAD" "$STEPS"
expect fail 'a new before hook' "$f" "the job sign does not run the before hook: make man"
# shellcheck disable=SC2016
printf 'before:\n  hooks:\n    - go run ./cmd/pco docs man {{ .Version }}\n' >"$CONFIG"
expect fail 'a hook with a template the job cannot fill' "$f" 'the before hook "go run ./cmd/pco docs man {{ .Version }}" has a template the job sign cannot fill'
CONFIG=

expect pass 'the release workflow of the repository' "$WORKFLOW"

# The step "Read the tag" of the workflow of the repository, run as the
# runner runs it, in a repository with the tags a release may be pushed with.
CASE=
RC=0
OUT=
LOG=
READ=$ROOT/read.sh
script "$(job "$WORKFLOW" build)" "Read the tag" >"$READ"
REPO=$ROOT/repo
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
gitc() {
	git -C "$REPO" -c user.name=Test -c user.email=test@example.invalid -c commit.gpgSign=false -c tag.gpgSign=false "$@"
}
git init -q "$REPO"
gitc commit -q --allow-empty -m one
gitc tag -a v0.1.0 -m "pco 0.1.0"
gitc tag -a v0.1.1 -m "pco 0.1.1" -m "Snapshot: 20261102T000000Z"
gitc tag v0.1.2
gitc tag -a v0.1.3 -m "pco 0.1.3" -m "Snapshot: yesterday"
gitc tag -a v0.1.4 -m "pco 0.1.4" -m "Snapshot: 20261102T000000Z" -m "Snapshot: 20261201T000000Z"
gitc tag -a --cleanup=verbatim v0.1.5 -m "pco 0.1.5" -m "What changed, at length." -m "Snapshot: 20261102T000000Z "$'\r'

# read_tag <tag> [input]: leaves RC, OUT, what the step wrote to
# GITHUB_OUTPUT, and LOG, what it printed.
read_tag() {
	: >"$ROOT/output"
	: >"$ROOT/summary"
	RC=0
	(cd "$REPO" && TAG=$1 SNAPSHOT=${2:-} GITHUB_OUTPUT=$ROOT/output GITHUB_STEP_SUMMARY=$ROOT/summary \
		bash --noprofile --norc -eo pipefail "$READ") >"$ROOT/log" 2>&1 || RC=$?
	OUT=$(<"$ROOT/output")
	LOG=$(<"$ROOT/log")
}

# assert <what holds> <command...>
assert() {
	local what=$1
	shift
	CHECKS=$((CHECKS + 1))
	if ! "$@"; then
		FAILS=$((FAILS + 1))
		printf 'FAIL [%s] %s\n    rc: %s\n    output: %s\n    log: %s\n' "$CASE" "$what" "$RC" "$OUT" "$LOG"
	fi
}

rc_is() { [[ $RC == "$1" ]]; }
is() { [[ $1 == "$2" ]]; }
contains() { [[ $1 == *"$2"* ]]; }

CASE='the step that reads the tag'
assert "is in the job build" test -s "$READ"

CASE='a tag that names its snapshot'
read_tag v0.1.1
assert "passes" rc_is 0
assert "with the snapshot of the tag" is "$OUT" "snapshot=20261102T000000Z"

CASE='a tag without a snapshot'
read_tag v0.1.0
assert "passes" rc_is 0
assert "with none, for pin.conf" is "$OUT" "snapshot="

CASE='the input and a tag that names a snapshot'
read_tag v0.1.1 20261201T000000Z
assert "passes" rc_is 0
assert "with the snapshot of the input" is "$OUT" "snapshot=20261201T000000Z"

CASE='a snapshot after other lines, with a space and a carriage return after it'
read_tag v0.1.5
assert "passes" rc_is 0
assert "with that snapshot" is "$OUT" "snapshot=20261102T000000Z"

CASE='a lightweight tag'
read_tag v0.1.2
assert "fails" rc_is 1
assert "and says so" contains "$LOG" "v0.1.2 is not an annotated tag"
assert "and gives nothing on" is "$OUT" ""
read_tag v0.1.2 20261201T000000Z
assert "fails with an input too" rc_is 1

CASE='a tag that is not there'
read_tag v9.9.9
assert "fails" rc_is 1

CASE='a tag whose snapshot is not one'
read_tag v0.1.3
assert "fails" rc_is 1
assert "naming what it found" contains "$LOG" "'yesterday'"
assert "and gives nothing on" is "$OUT" ""

CASE='a tag with two snapshots'
read_tag v0.1.4
assert "fails" rc_is 1
assert "and says so" contains "$LOG" "more than one"

CASE='an input that is not a snapshot'
read_tag v0.1.0 latest
assert "fails" rc_is 1
assert "naming it" contains "$LOG" "'latest'"

# The step of the job publish that tells whether the release is the latest,
# run as the runner runs it, in a repository with a release, a candidate and
# a fix for an older line. What it writes is what site and pages wait for.
LATEST=$ROOT/latest.sh
script "$(job "$WORKFLOW" publish)" "Check whether this is the latest release" >"$LATEST"
REPO=$ROOT/releases
git init -q "$REPO"
mkdir "$REPO/packaging"
cp "$HERE/is-latest.sh" "$REPO/packaging/is-latest.sh"
gitc commit -q --allow-empty -m one
for tag in v1.0.0 v1.0.1 v1.1.0 v1.2.0-rc.1; do
	gitc tag "$tag"
done

# is_latest <tag>: leaves RC, OUT and LOG as read_tag does.
is_latest() {
	: >"$ROOT/output"
	RC=0
	(cd "$REPO" && TAG=$1 GITHUB_OUTPUT=$ROOT/output \
		bash --noprofile --norc -eo pipefail "$LATEST") >"$ROOT/log" 2>&1 || RC=$?
	OUT=$(<"$ROOT/output")
	LOG=$(<"$ROOT/log")
}

CASE='the step that tells whether the release is the latest'
assert "is in the job publish" test -s "$LATEST"

CASE='the highest final version'
is_latest v1.1.0
assert "passes" rc_is 0
assert "is the latest" is "$OUT" "latest=true"

CASE='a fix for an older line'
is_latest v1.0.1
assert "passes" rc_is 0
assert "is not the latest" is "$OUT" "latest=false"

CASE='a release candidate of a newer version'
is_latest v1.2.0-rc.1
assert "passes" rc_is 0
assert "is not the latest" is "$OUT" "latest=false"

CASE='a newer version than the one before'
gitc tag v1.2.0
is_latest v1.2.0
assert "passes" rc_is 0
assert "is the latest" is "$OUT" "latest=true"
is_latest v1.1.0
assert "passes for the one before" rc_is 0
assert "which is not the latest any more" is "$OUT" "latest=false"

printf '%d checks, %d failed\n' "$CHECKS" "$FAILS"
if [[ $FAILS != 0 ]]; then
	exit 1
fi
