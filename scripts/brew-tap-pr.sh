#!/usr/bin/env bash
# Land a tap update through the merge queue instead of pushing to main.
#
# Usage:
#   scripts/brew-tap-pr.sh branch  <tag>   # switch to this tag's tap branch
#   scripts/brew-tap-pr.sh publish <tag>   # commit + push + PR + auto-merge
#
# A direct push to `main` invalidates the merge group at the head of the
# queue and makes it rebuild — about 20 to 25 minutes, paid by whichever PR
# is first in line, once per release on top of the release commit's own
# replay. So the tap change takes an ordered slot of its own: one branch per
# tag, one PR, auto-merge armed.
#
# Called by brew-update.yml once a night, and by its backfill dispatch, so
# both subcommands are idempotent: a second run for the same tag builds on
# that tag's branch while its PR is open, and rebuilds the branch from main
# otherwise — branches outlive their merged or failed PR here
# (delete_branch_on_merge is off), and building on one would carry main's
# past into the next PR. A PR opens only for what main does not carry yet.
#
# `branch` expects a git repo on `main` with `origin` fetchable and `gh`
# authenticated (it reads whether the tag's PR is open); `publish`
# expects the tap files already rewritten by scripts/update-brew-tap.sh, a
# git identity, push credentials on `origin`, and `gh` authenticated as an
# identity that may open a PR (GH_TOKEN).
set -euo pipefail

cmd="${1:?usage: brew-tap-pr.sh <branch|publish> <tag>}"
tag="${2:?usage: brew-tap-pr.sh <branch|publish> <tag>}"
branch="chore/brew-tap-${tag}"

# version_lt A B: version A sorts strictly before version B under `sort -V`,
# the order the workflow's downgrade guard compares tags in.
version_lt() {
  [ "$1" != "$2" ] && printf '%s\n%s\n' "$1" "$2" | sort -V -C
}

# This repository's own open PR for BRANCH, empty if none. `gh pr list
# --head` matches the branch NAME, whoever owns the head: a pull request from
# a fork named like a tap branch — anyone can open one on a public
# repository — is never this repository's tap PR.
own_open_pr() {
  gh pr list --state open --head "$1" --json number,isCrossRepository \
    --jq '[.[] | select(.isCrossRepository | not)][0].number // empty'
}

# The tip of origin's refs/heads/BRANCH, empty if absent. `git ls-remote`
# matches ref names by their tail (a `backup/<branch>` matches too), so the
# ref is compared whole.
remote_tip() {
  git ls-remote origin "refs/heads/$1" | awk -v ref="refs/heads/$1" '$2 == ref { print $1 }'
}

# version_of TEXT: the first `version "…"` value in TEXT, empty if none.
version_of() {
  sed -n 's/^[[:space:]]*version[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$1" | head -n1
}

case "$cmd" in
branch)
  # Build on this tag's own branch while its PR is open, so a second pass
  # adds to that slot instead of dropping the first pass's formula. Any other
  # remote branch of that name is a leftover — its PR merged, closed, or never
  # opened — and building on it would carry main's past into the next PR:
  # start from main (checked out) instead; publish replaces the leftover.
  # The remote branch, when there is one, is fetched either way: its tip is
  # what publish's lease expects, so a push made to it after this point is
  # refused rather than overwritten.
  open_pr="$(own_open_pr "$branch")"
  tip="$(remote_tip "$branch")"
  if [ -n "$tip" ]; then
    git fetch --quiet origin "+refs/heads/${branch}:refs/remotes/origin/${branch}"
  fi
  if [ -n "$open_pr" ] && [ -n "$tip" ]; then
    git checkout -B "$branch" "refs/remotes/origin/${branch}"
    echo "on ${branch}, the branch of open PR #${open_pr}"
  else
    git checkout -B "$branch"
    echo "on ${branch}, from $(git rev-parse --short HEAD)"
  fi
  ;;

publish)
  # A pre-release never takes the tap: `sort -V` orders a `-rc` suffix ABOVE
  # its own final, so an open rc tap PR would silently block that final's
  # update. Refuse loudly instead — the final's run publishes normally.
  case "${tag#v}" in
    *-*) echo "not tapping ${tag}: pre-release tags do not take the tap; wait for the final"; exit 0 ;;
  esac

  # Every open tap PR as "<number> <tag>", read once: the guard below and the
  # close at the end judge the same list.
  open_taps="$(gh pr list --state open --limit 100 --json number,headRefName,isCrossRepository \
    --jq '.[] | select(.isCrossRepository | not) | select(.headRefName | startswith("chore/brew-tap-")) | "\(.number) \(.headRefName | ltrimstr("chore/brew-tap-"))"')"

  # A run for an older tag — a re-run, a dispatch, a delayed run — can come
  # after newer tap work was opened or published. Its formula would walk the
  # tap back, so it ends here: nothing pushed, no PR opened, none closed.
  git fetch --quiet origin main
  # An absent formula path means nothing is published yet (first publication,
  # or a moved formula) — not a fallback for a failed fetch: `set -e` already
  # killed the run above if the fetch itself failed.
  formula="$(git show origin/main:Formula/iterion.rb 2>/dev/null || true)"
  published="$(sed -n '/^[[:space:]]*version[[:space:]]*"/{s/^[[:space:]]*version[[:space:]]*"\([^"]*\)".*/\1/p;q;}' <<<"$formula")"
  newer=""
  if [ -n "$published" ] && version_lt "${tag#v}" "$published"; then
    newer="main publishes ${published}"
  fi
  while read -r number other; do
    [ -n "$number" ] || continue
    if version_lt "${tag#v}" "${other#v}"; then
      newer="${newer:+${newer}; }tap PR #${number} is open for ${other}"
    fi
  done <<<"$open_taps"
  if [ -n "$newer" ]; then
    echo "not publishing ${tag}, older than the tap's newer work: ${newer}"
    exit 0
  fi

  # Nothing below main's versions leaves this run: an open PR's branch can sit
  # under a main that moved since, and pushing it would walk the tap back.
  # Refused before any push, and without the sweep below — this run lands
  # nothing, so it supersedes nothing.
  behind=""
  for f in Formula/iterion.rb Cask/iterion-desktop.rb; do
    ours="$(version_of "$(cat "$f" 2>/dev/null || true)")"
    theirs="$(version_of "$(git show "origin/main:$f" 2>/dev/null || true)")"
    if [ -n "$ours" ] && [ -n "$theirs" ] && version_lt "$ours" "$theirs"; then
      behind="${behind:+${behind}; }${f} ${ours} below main's ${theirs}"
    fi
  done
  if [ -n "$behind" ]; then
    echo "not publishing ${tag}: it would walk the tap back (${behind})"
    exit 0
  fi

  pushed=""
  git add Formula/iterion.rb Cask/iterion-desktop.rb
  if git diff --staged --quiet; then
    # Not an error: the second pipeline's pass sees artefacts the first
    # already wrote, and a re-run of the same pass sees its own commit.
    echo "tap already up to date for ${tag}, nothing to commit"
  else
    git commit -m "chore(brew): update tap to ${tag}"
    # The branch may have been rebuilt from main over a leftover of the same
    # name: replace it — but only the tip `branch` saw (or its absence), so a
    # push made to it since is refused, never overwritten.
    seen="$(git rev-parse --quiet --verify "refs/remotes/origin/${branch}" || true)"
    git push --force-with-lease="refs/heads/${branch}:${seen}" origin "$branch"
    pushed=1
  fi

  # No `[skip ci]` anywhere in that message on purpose: the required checks
  # must REPORT on this PR, or auto-merge waits on contexts that never
  # report. The heavy ones report by skipping, keyed on the PR title below —
  # tests.yml's header says why, and `brand` verifies the PR touches the tap
  # alone. Keep the title's prefix: internal/ciguard pins it to that skip.

  # One open PR per tag. `--head` matches the branch, so a merged PR whose
  # branch was recreated does not count.
  existing="$(own_open_pr "$branch")"
  if [ -n "$existing" ]; then
    echo "PR #${existing} already open for ${branch}${pushed:+; the push above updated it}"
  else
    # Read into a variable first: a failed ls-remote inside `[ … ]` would read
    # as an absent branch and end the run green, with a branch just pushed.
    if [ -z "$pushed" ]; then
      tip="$(remote_tip "$branch")"
      if [ -z "$tip" ]; then
        echo "nothing pushed for ${tag} and no open PR — the tap was already current"
        exit 0
      fi
    fi
    # A leftover branch that nothing was pushed over carries no news: GitHub
    # would accept a PR from it that changes nothing, since a squash merge
    # never makes a branch an ancestor of main. Open one only for what main
    # does not carry yet (origin/main was fetched by the guard above).
    if git diff --quiet origin/main HEAD -- Formula/iterion.rb Cask/iterion-desktop.rb; then
      echo "main already carries ${branch}'s tap files — no PR to open"
    else
      url="$(gh pr create \
        --base main \
        --head "$branch" \
        --title "chore(brew): update tap to ${tag}" \
        --body "Homebrew formula + cask for \`${tag}\`, generated by \`scripts/update-brew-tap.sh\`.

Landing through the merge queue rather than as a direct push to \`main\`: a
direct push invalidates the group at the head of the queue and makes every
PR behind it rebuild.")"
      [ -n "$url" ] || { echo "::error::gh pr create printed no pull request URL" >&2; exit 1; }

      # Auto-merge is what puts it in the queue. A failure here is loud on
      # purpose: a tap PR nobody merges is a stale formula plus an open PR the
      # next release has to close. Armed by the URL just created, never by
      # branch name, which a fork's PR can share.
      gh pr merge "$url" --auto --squash
    fi
  fi

  # A tap PR for an EARLIER tag is superseded, not to be merged: its formula
  # would downgrade the published version. Close it and drop its branch.
  while read -r number other; do
    [ -n "$number" ] || continue
    version_lt "${other#v}" "${tag#v}" || continue
    echo "closing superseded tap PR #${number} (${other})"
    gh pr close "$number" --delete-branch \
      --comment "Superseded by the tap update for \`${tag}\`."
  done <<<"$open_taps"
  ;;

*)
  echo "unknown subcommand: ${cmd}" >&2
  exit 2
  ;;
esac
