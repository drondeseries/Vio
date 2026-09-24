#!/usr/bin/env bash
# Resolve the commit whose committed OpenAPI document is the base for the
# native API v2 semantic diff (make verify-apiv2-contract).
#
# The default rule is `git merge-base BASE_REF HEAD`: the common ancestor is the
# contract the branch diverged from, so the diff is exactly what the branch
# changes. That rule is wrong for a merge-commit HEAD. A merge's merge-base with
# its own base branch can land on a commit *inside* the merged work — after the
# contract change was already committed — so the base document already carries
# the change and the diff is empty. af22a65e (the fork-delta rebase) is the
# case that slipped through: its contract breaks were invisible because the
# merge-base landed on a revision that had them.
#
# When HEAD is a merge, the base is therefore the explicit BASE_REF (what CI's
# pull request is measured against) when it names a different commit, falling
# back to the merge's first parent. A merge of the base branch into a feature
# branch keeps its first parent on the feature side, and a base-branch merge of
# a feature keeps its first parent on the base side; either way the diff spans
# the merged work rather than collapsing to nothing.
#
# Usage: scripts/apiv2-contract-base.sh [BASE_REF] [REVISION]
#   BASE_REF  the comparison branch or commit (default origin/main)
#   REVISION  the revision to anchor (default HEAD)
# Prints the resolved commit sha on stdout.
set -euo pipefail

base_ref=${1:-origin/main}
revision=${2:-HEAD}

if ! git rev-parse --verify --quiet "${revision}^{commit}" >/dev/null; then
	echo "apiv2-contract-base: revision ${revision} is not a commit" >&2
	exit 1
fi
rev=$(git rev-parse "${revision}^{commit}")

# `git rev-list --parents -n 1` lists the commit then its parents, so more than
# two fields means the commit is a merge.
parent_fields=$(git rev-list --parents -n 1 "$rev")
# shellcheck disable=SC2206 # word splitting on the sha list is intended
parents=($parent_fields)

if [ "${#parents[@]}" -gt 2 ]; then
	if candidate=$(git rev-parse --verify --quiet "${base_ref}^{commit}" 2>/dev/null) && [ "$candidate" != "$rev" ]; then
		printf '%s\n' "$candidate"
		exit 0
	fi
	# BASE_REF is HEAD itself (a base-branch push) or unavailable: diff the
	# merge against its first parent so the merged change is still measured.
	printf '%s\n' "${parents[1]}"
	exit 0
fi

git merge-base "$base_ref" "$rev"
