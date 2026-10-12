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
# Called once per release pipeline (release.yml and desktop-release.yml each
# fire the workflow), so both subcommands are idempotent: the second call
# re-uses the first call's branch and PR, adding the desktop cask hash to
# the same slot.
#
# `branch` expects a git repo on `main` with `origin` fetchable; `publish`
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

case "$cmd" in
branch)
  # Build on this tag's own branch when it already exists (the first
  # pipeline opened it), so the second pass adds to that slot instead of
  # dropping the first pass's formula.
  if git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
    git fetch origin "$branch"
    git checkout -B "$branch" "origin/$branch"
  else
    git checkout -B "$branch"
  fi
  echo "on ${branch}"
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
  open_taps="$(gh pr list --state open --limit 100 --json number,headRefName \
    --jq '.[] | select(.headRefName | startswith("chore/brew-tap-")) | "\(.number) \(.headRefName | ltrimstr("chore/brew-tap-"))"')"

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

  git add Formula/iterion.rb Cask/iterion-desktop.rb
  if git diff --staged --quiet; then
    # Not an error: the second pipeline's pass sees artefacts the first
    # already wrote, and a re-run of the same pass sees its own commit.
    echo "tap already up to date for ${tag}, nothing to commit"
  else
    git commit -m "chore(brew): update tap to ${tag}"
    git push origin "$branch"
  fi

  # No `[skip ci]` anywhere in that message on purpose: the required checks
  # must run on this PR, or auto-merge waits on contexts that never report.

  # One open PR per tag. `--head` matches the branch, so a merged PR whose
  # branch was recreated does not count.
  existing="$(gh pr list --state open --head "$branch" --json number --jq '.[0].number // empty')"
  if [ -n "$existing" ]; then
    echo "PR #${existing} already open for ${branch}; the push above updated it"
  else
    if ! git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
      echo "nothing pushed for ${tag} and no open PR — the tap was already current"
      exit 0
    fi
    gh pr create \
      --base main \
      --head "$branch" \
      --title "chore(brew): update tap to ${tag}" \
      --body "Homebrew formula + cask for \`${tag}\`, generated by \`scripts/update-brew-tap.sh\`.

Landing through the merge queue rather than as a direct push to \`main\`: a
direct push invalidates the group at the head of the queue and makes every
PR behind it rebuild."

    # Auto-merge is what puts it in the queue. A failure here is loud on
    # purpose: a tap PR nobody merges is a stale formula plus an open PR the
    # next release has to close.
    gh pr merge "$branch" --auto --squash
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
