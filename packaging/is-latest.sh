#!/usr/bin/env bash
#
# Succeeds when the given tag is the highest final version, vX.Y.Z, among the v*
# tags of the repository in the current directory. The release workflow marks a
# release as the latest only then: a fix released for an older line after a
# newer version exists must not become what the installer picks.
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

# sed and not head: head closing the pipe early would fail the pipeline.
highest=$(git tag --list 'v*' --sort=-v:refname | grep -E "$final" | sed -n 1p || true)
[[ $highest == "$tag" ]]
