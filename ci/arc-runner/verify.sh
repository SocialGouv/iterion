#!/usr/bin/env bash
set -euo pipefail
image=${1:?usage: verify.sh IMAGE}
smoke_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/smoke" && pwd)
task_goroot=$(go env GOROOT)
# Mount the caller's pinned setup-go/devbox toolchain read-only. The compiler
# and libc used by cgo come from the image under test, not from the host.
docker run --rm --network none --read-only \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=1g \
  --volume "$task_goroot:/opt/go:ro" \
  --volume "$smoke_dir:/src:ro" --workdir /src \
  --env GOROOT=/opt/go --env GOPATH=/tmp/gopath --env GOCACHE=/tmp/go-cache \
  --env GOENV=off --env GOTOOLCHAIN=local --env CGO_ENABLED=1 --env CC=gcc \
  --env PATH=/opt/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  --entrypoint /bin/sh "$image" -ec '
    test "$(id -u)" = 1001
    go test -race -count=1 ./...
  '
