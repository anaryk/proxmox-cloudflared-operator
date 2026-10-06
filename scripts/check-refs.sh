#!/usr/bin/env bash
#
# Fails when a file of the repository cites material that is not in it: a ruling,
# a numbered section of a spec, a gate, a failure mode, a plan or a task of the
# planning, an experiment of the lab, a numbered finding. A reader cannot follow
# such a citation, and it reads as a process note and not as the reason of the
# code. The comment, the test name or the page says the reason in its own words.
#
# It reads the tracked files of the repository it runs in, and the staged ones.
# Each pattern below is one shape of citation the repository had before this
# check. A shape it does not know is for review to catch, and for a new pattern.
#
# Usage: scripts/check-refs.sh

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# A word starts and ends at a character that cannot be part of one.
wl='(^|[^[:alnum:]_])'
wr='($|[^[:alnum:]_])'
# A label such as G2 is not one inside a path, a MAC address, a percent
# encoding or a number: its neighbours must not be among those characters.
ll='(^|[^[:alnum:]_%./:+=-])'
lr='($|[^[:alnum:]_%/+=-])'
# What a comment or a string starts with.
opens="(//|#|/[*]|^[[:space:]]*[*]|[\"'\`])"

names=(
	'ruling'
	'spec'
	'gate'
	'failure mode'
	'plan or task'
	'report'
	'lab'
	'recommendation or numbered reproduction'
	'the brief'
	'label'
	'label opening a comment or a string'
)
patterns=(
	"${wl}[Rr]ulings?${wr}"
	"spec-ui|${wl}spec (v[0-9]|section|§)"
	"${wl}[Gg]ate [0-9]"
	"${wl}[Ff]ailure mode [0-9]"
	"${wl}([Pp]lans?|[Tt]ask) [0-9]"
	"${wl}[Rr]eport [0-9]"
	"${wl}([Ee]xperiment|[Ll]ab) [A-Z]?[0-9]|${wl}[Ll]ab finding"
	"${wl}([Rr]ecommended|[Ss]uggestion) [0-9]|${wl}[Rr]eproduced \\([a-z0-9]\\)"
	"${wl}([Ss]ee|[Pp]er|[Ii]n|[Ff]rom|[Oo]f) the brief${wr}"
	"${ll}([ADGHPQ][0-9]{1,2}|R([013-9]|[0-9]{2}))${lr}|${ll}G[0-9]-[0-9]"
	"${opens}[[:space:]]*[A-Z][0-9]{1,2}:[[:space:]]"
)

# The lock files hold hashes, and this script and its test hold the patterns.
skip=(
	':(exclude)go.sum'
	':(exclude)web/package-lock.json'
	':(exclude)scripts/check-refs.sh'
	':(exclude)scripts/check-refs_test.sh'
)

found=0
for i in "${!patterns[@]}"; do
	rc=0
	hits=$(git grep -n -I -E -e "${patterns[$i]}" -- . "${skip[@]}") || rc=$?
	case $rc in
	0)
		printf '%s\n' "$hits" | sed "s/^/${names[$i]}: /" >&2
		found=1
		;;
	1) ;; # nothing matches: what passes
	*)
		printf 'check-refs: git grep failed (%s) on the pattern for %s\n' "$rc" "${names[$i]}" >&2
		exit 2
		;;
	esac
done

if ((found)); then
	printf '%s\n' 'check-refs: the lines above cite material that is not in the repository.' \
		'Say the reason in the comment, the test name or the page instead.' >&2
	exit 1
fi
