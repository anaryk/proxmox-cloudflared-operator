#!/usr/bin/env bash
#
# Succeeds when the given tag is the highest final version, vX.Y.Z, among the v*
# tags of the repository in the current directory. The release workflow marks a
# release as the latest only then: a fix released for an older line after a
# newer version exists must not become what the installer picks.
#
# Exit status 1 means not the latest; 2 means no answer, when the tags could
# not be read, so that a caller never takes a broken repository for an older
# line.
#
# Usage: packaging/is-latest.sh <tag>

set -euo pipefail

if [[ $# != 1 ]]; then
	printf 'usage: is-latest.sh <tag>\n' >&2
	exit 2
fi
tag=$1
final='^v[0-9]+\.[0-9]+\.[0-9]+$'
if ! [[ $tag =~ $final ]]; then
	exit 1
fi

if ! tags=$(git tag --list 'v*' --sort=-v:refname); then
	exit 2
fi
# No final version among the tags is an answer: this one is not the latest.
highest=$(grep -E "$final" <<<"$tags" | sed -n 1p || true)
[[ $highest == "$tag" ]]
