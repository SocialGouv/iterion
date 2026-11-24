#!/usr/bin/env bash
set -euo pipefail
image=${1:?usage: verify.sh IMAGE}
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# Resolved, not spelled with `..`: the bind mounts below are host paths the
# daemon takes literally.
repo=$(cd -- "$here/../.." && pwd)
# The Go the image must bake is the one setup-go reads from go.mod: the
# `toolchain` line when there is one, the `go` line otherwise.
want_go=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); t = $2 } $1 == "go" && g == "" { g = $2 } END { print (t != "" ? t : g) }' "$here/../../go.mod")
[ -n "$want_go" ] || { echo "no go line in go.mod" >&2; exit 1; }
# Everything below runs as the runner user, offline, on a read-only root: the
# toolchains, the compiler and libc all come from the image under test. One
# check per line — under `bash -e`, a failing `test` that is not the last of
# an && list does not stop the script.
docker run --rm --network none --read-only \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=1g \
  --volume "$here/smoke:/src:ro" --workdir /src \
  --volume "$repo/scripts/ci/verify-baked-actions.sh:/verify-baked-actions.sh:ro" \
  --volume "$here/baked-actions.txt:/baked-actions.txt:ro" \
  --env WANT_GO="$want_go" \
  --entrypoint /bin/bash "$image" -ec '
    test "$(id -u)" = 1001
    test "$RUNNER_TOOL_CACHE" = /opt/hostedtoolcache
    # setup-go and setup-node add versions there: the runner owns all of it.
    test -z "$(find "$RUNNER_TOOL_CACHE" ! -uid 1001 -print -quit)"
    . "$RUNNER_TOOL_CACHE/iterion-ci-runner.env"
    test "$GO_VERSION" = "$WANT_GO"
    go_dir="$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64"
    test -f "$go_dir.complete"
    test "$("$go_dir/bin/go" env GOVERSION)" = "go$GO_VERSION"
    node_dirs=("$RUNNER_TOOL_CACHE"/node/*/x64)
    test "${#node_dirs[@]}" = 1
    test "${node_dirs[0]}" = "$RUNNER_TOOL_CACHE/node/$NODE_VERSION/x64"
    test -f "${node_dirs[0]}.complete"
    test "$("${node_dirs[0]}/bin/node" --version)" = "v$NODE_VERSION"
    # The workflows ask for node-version: 24.
    case "$NODE_VERSION" in 24.*) ;; *) echo "baked Node $NODE_VERSION is not a 24.x" >&2; exit 1 ;; esac
    # The baked actions, resolved with no network at all — this run has none.
    # The cache must be outside _work (ARC mounts a volume over it, which
    # would hide everything baked there), the image must have baked from the
    # manifest this tree ships, and every `uses:` of the workflows must come
    # out of the cache: the negative control is removing one archive, which
    # fails this step naming the action.
    test "$ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" = /opt/actions-archive-cache
    case "$ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" in /home/runner/_work*) echo "the action archive cache sits under _work, where ARC mounts a volume over it" >&2; exit 1 ;; esac
    cmp /baked-actions.txt "$RUNNER_TOOL_CACHE/iterion-ci-runner-actions.txt"
    bash /verify-baked-actions.sh "$ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" /baked-actions.txt
    test -z "$(find "$ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" ! -uid 1001 -print -quit)"
    export GOROOT="$go_dir" PATH="$go_dir/bin:$PATH" GOPATH=/tmp/gopath \
      GOCACHE=/tmp/go-cache GOENV=off GOTOOLCHAIN=local CGO_ENABLED=1 CC=gcc
    go test -race -count=1 ./...
  '
