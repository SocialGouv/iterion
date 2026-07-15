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
#   <sha>        claw-code-go commit to pin (default: HEAD of the
#                .works/claw-code-go worktree)
#   --no-commit  stage go.mod/go.sum/vendor but leave the commit to you
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLAW_DIR="${CLAW_DIR:-$REPO_ROOT/.works/claw-code-go}"
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
