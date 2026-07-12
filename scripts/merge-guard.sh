#!/usr/bin/env bash
# merge-guard — the merge-queue discipline applied at our own merge step.
#
# CI validates a PR against main FROZEN at branch time (the merge-ref), never
# against OTHER in-flight PRs. Two PRs can each be green yet break main when
# combined — a semantic merge conflict git doesn't textually flag (observed
# 2026-07-12: #120 added a param to a function, #121 added a caller with the
# old signature; each merge-ref compiled, combined main did not).
#
# This guard closes that class for the PRs WE merge: it rebases the PR onto the
# CURRENT origin/main (all already-merged PRs included), builds + runs the
# deterministic gates on the COMBINED tree, and only merges if green — the same
# check a native merge queue automates. Use it instead of a bare `gh pr merge`.
#
# Usage: scripts/merge-guard.sh <pr-number> [--no-merge]
#   --no-merge : run the combined-build check + report, but don't merge.
#
# Requires: a clean working tree (it uses a throwaway worktree, so your
# checkout is untouched), gh, and the devbox toolchain.
set -euo pipefail

PR="${1:?usage: merge-guard.sh <pr-number> [--no-merge]}"
NO_MERGE=0
[ "${2:-}" = "--no-merge" ] && NO_MERGE=1

REPO="${MERGE_GUARD_REPO:-SocialGouv/iterion}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WT="$(mktemp -d)/merge-guard-$PR"

cleanup() { git -C "$ROOT" worktree remove --force "$WT" 2>/dev/null || true; rm -rf "$(dirname "$WT")" 2>/dev/null || true; }
trap cleanup EXIT

branch="$(gh pr view "$PR" --repo "$REPO" --json headRefName --jq .headRefName)"
echo "→ PR #$PR head: $branch"

git -C "$ROOT" fetch origin -q "$branch" main
git -C "$ROOT" worktree add -q "$WT" "origin/$branch"
cd "$WT"

echo "→ rebasing #$PR onto current origin/main (the combined tree CI never tested)"
if ! git rebase origin/main; then
  git rebase --abort 2>/dev/null || true
  echo "✗ CONFLICT rebasing #$PR on origin/main — resolve on the branch first, then re-run." >&2
  exit 2
fi

echo "→ building the COMBINED tree (the #120×#121 check)"
if ! devbox run -- go build ./... ; then
  echo "✗ #$PR does NOT build against current main — a semantic merge conflict. Do not merge." >&2
  exit 3
fi

echo "→ deterministic gates on the combined tree"
if ! devbox run -- go test ./pkg/store/mongo/ -run 'TestDeleteRunCoversEveryPerRunCollection' -count=1 ; then
  echo "✗ #$PR fails a deterministic parity gate against current main." >&2
  exit 4
fi

echo "✓ #$PR builds + passes the deterministic gates against current origin/main."

if [ "$NO_MERGE" = 1 ]; then
  echo "→ --no-merge: not merging. The rebased branch was not pushed."
  exit 0
fi

# Push the rebased branch so the PR merges the combined-validated state, then merge.
echo "→ pushing the rebased branch + squash-merging"
git push -f origin "HEAD:$branch"
gh pr merge "$PR" --repo "$REPO" --squash --delete-branch
echo "✓ #$PR merged (combined-tree validated)."
