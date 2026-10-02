#!/usr/bin/env bash
# Bump the vendored claw-code-go pin — the ONLY supported way.
#
# Hand-writing the pseudo-version breaks `go mod verify` whenever the
# timestamp is stamped in local time instead of the commit's UTC time
# (go rejects it with "does not match version-control timestamp", which
# turns vendor-check red on main and on every PR merge-ref). `go get`
# computes the canonical UTC pseudo-version, so this script wraps it.
#
# Usage: scripts/bump-claw.sh [<sha>] [--no-commit]
#   <sha>        claw-code-go commit to pin (default: HEAD of the primary
#                checkout's .works/claw-code-go clone, which must be on master)
#   --no-commit  stage go.mod/go.sum/vendor but leave the commit to you
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# .works/ lives in the primary checkout only: resolve it through the common
# git dir so the script also finds it when run from a linked worktree. Two
# assignments, so that set -e stops on a git failure instead of `dirname ""`.
COMMON_DIR="$(git -C "$REPO_ROOT" rev-parse --path-format=absolute --git-common-dir)"
PRIMARY_ROOT="$(dirname "$COMMON_DIR")"
CLAW_DIR="${CLAW_DIR:-$PRIMARY_ROOT/.works/claw-code-go}"
MODULE=github.com/SocialGouv/claw-code-go

sha=""
commit=1
for arg in "$@"; do
  case "$arg" in
    --no-commit) commit=0 ;;
    *) sha="$arg" ;;
  esac
done

if [ -z "$sha" ]; then
  [ -d "$CLAW_DIR" ] || { echo "error: no sha given and $CLAW_DIR not found" >&2; exit 1; }
  # Every worktree's sessions share this clone: without a sha, pin its master,
  # never whatever branch a session left checked out there.
  head_ref=$(git -C "$CLAW_DIR" rev-parse --abbrev-ref HEAD)
  [ "$head_ref" = master ] || { echo "error: no sha given and $CLAW_DIR is on $head_ref, not master; pass the sha to pin" >&2; exit 1; }
  sha=$(git -C "$CLAW_DIR" rev-parse HEAD)
fi
short=${sha:0:12}

# The pin must be resolvable by the Go module proxy: push claw first if
# the commit isn't reachable from any remote ref.
if [ -d "$CLAW_DIR" ] && git -C "$CLAW_DIR" cat-file -e "$sha" 2>/dev/null; then
  if ! git -C "$CLAW_DIR" branch -r --contains "$sha" | grep -q .; then
    echo "→ $short not on origin; pushing claw master"
    git -C "$CLAW_DIR" push origin master
  fi
fi

cd "$REPO_ROOT"
subject=""
if [ -d "$CLAW_DIR" ]; then
  subject=$(git -C "$CLAW_DIR" log -1 --format=%s "$sha" 2>/dev/null || true)
fi

echo "→ go get $MODULE@$short"
GOFLAGS=-mod=mod go get "$MODULE@$sha"
go mod tidy
go mod vendor
go mod verify
go build ./... >/dev/null
echo "→ pinned: $(grep "$MODULE" go.mod)"

git add go.mod go.sum vendor/
if [ "$commit" = 1 ]; then
  msg="chore(vendor): bump claw-code-go"
  [ -n "$subject" ] && msg="$msg — $subject"
  git commit -m "$msg"
  echo "→ committed: $(git log --oneline -1)"
else
  echo "→ staged (no commit requested)"
fi
