#!/usr/bin/env bash
# The bench image a REMOTE cluster can pull (#3299, ADR-165).
#
# WHY THIS EXISTS. `codemode-cluster-bench.sh` puts the shipped image in a real
# Pod — but it builds its cluster with `kind`, hands the bench binary to the
# node through `extraMounts`, and the pod reads it from a `hostPath`. None of
# that survives a cluster boundary: a production node will not accept a hostPath
# to a directory on an operator's laptop, and nothing here may touch the node.
# The bench binary therefore has to travel INSIDE an image the cluster can pull.
#
# WHAT IT BUILDS. `FROM <the deployment's own image>` plus `/bench/inpodbench`,
# and nothing else. The base is pinned by digest and is the image the deployment
# actually runs: the posture measured must be the deployment's, and a bench
# image built on some other base would answer for a container nobody runs.
#
# ITS OUTPUT IS A DIGEST, on stdout, as `BENCH_IMAGE=…`. A caller passes that to
# `codemode-remote-bench.sh`. Passing a tag instead would let the image move
# under the verdict — the same reason every arm of this bench refuses tags.
#
# THE WITNESS LANE. `--load-kind <cluster>` exists so the remote driver can be
# exercised end to end without a registry: it loads the image into an existing
# kind cluster and prints a TAG, which the driver accepts only under
# `--pull-policy Never` (nothing pulls, so nothing can move). That lane proves
# the DRIVER; it says nothing about a production node.
set -Eeuo pipefail

say() { printf 'codemode-bench-image: %s\n' "$*" >&2; }
die() { say "$*"; exit 2; }

# ONE guard for every step not handled by name, as in the bench arms: a naked
# failure under `set -e` exits with the failing command's own status, and
# every failure of this script means the same thing — no image was produced.
# shellcheck disable=SC2154 # rc is assigned inside the trap string itself
trap 'rc=$?; [ "$BASHPID" -eq "$$" ] || exit "$rc"; say "no image was produced: a step failed at line $LINENO (exit $rc)"; exit 2' ERR

usage() {
	cat >&2 <<'USAGE'
usage: codemode-bench-image.sh --from <image@sha256:…> (--push <repo:tag> | --load-kind <cluster>) [options]

  --from        the deployment's image, pinned by DIGEST (required).
  --push        push the bench image here and print its digest (required unless
                --load-kind). The registry must be one the target cluster pulls
                from.
  --load-kind   the witness lane: load into an existing kind cluster instead of
                pushing, and print a TAG. Proves the driver, not a prod node.
  --platform    default linux/amd64 — the platform the cluster runs.
  --keep        leave the local image behind (default: remove it).

  ITERION_CI_GATE_EVIDENCE=<dir> keeps the build log and the Dockerfile.

  Emits on stdout: BENCH_IMAGE=<ref> (a digest, or a tag under --load-kind).
USAGE
	exit 2
}

FROM_IMAGE="" PUSH_REF="" LOAD_KIND="" PLATFORM="linux/amd64" KEEP=""
while [ $# -gt 0 ]; do
	case "$1" in
	--from)
		[ $# -ge 2 ] || die "--from needs a value"
		[ -z "$FROM_IMAGE" ] || die "--from given twice ('$FROM_IMAGE' then '$2'): refusing to guess which base you meant"
		FROM_IMAGE=$2
		shift 2
		;;
	--push)
		[ $# -ge 2 ] || die "--push needs a value"
		[ -z "$PUSH_REF" ] || die "--push given twice ('$PUSH_REF' then '$2')"
		PUSH_REF=$2
		shift 2
		;;
	--load-kind)
		[ $# -ge 2 ] || die "--load-kind needs a cluster name"
		LOAD_KIND=$2
		shift 2
		;;
	--platform)
		[ $# -ge 2 ] || die "--platform needs a value"
		PLATFORM=$2
		shift 2
		;;
	--keep) KEEP=1; shift ;;
	*) usage ;;
	esac
done

[ -n "$FROM_IMAGE" ] || usage
case "$FROM_IMAGE" in
*@sha256:*) ;;
*) die "--from must be pinned by digest, got '$FROM_IMAGE': an unbuilt base would let the measured posture move under the verdict" ;;
esac
# Exactly one destination. Both would build twice and print two answers; yet one
# more reproach: a caller reading the first line would take the kind TAG for a
# pushed DIGEST.
if [ -n "$PUSH_REF" ] && [ -n "$LOAD_KIND" ]; then
	die "--push and --load-kind are exclusive: one builds for a registry, the other for a local cluster"
fi
if [ -z "$PUSH_REF" ] && [ -z "$LOAD_KIND" ]; then
	usage
fi
# `docker push repo` (no tag) pushes `:latest`, a name meaning "whatever was
# last pushed here" — the opposite of what this script is for.
if [ -n "$PUSH_REF" ]; then
	case "${PUSH_REF##*/}" in
	*:*) ;;
	*) die "--push needs an explicit tag, got '$PUSH_REF': a bare repository pushes :latest" ;;
	esac
fi
command -v docker > /dev/null || die "docker is not on PATH, so nothing could be built"

cd "$(git rev-parse --show-toplevel)"
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-$(mktemp -d)}
# The bench binary is 14 MB: leaving it inside the work tree is litter, and
# `git status` in a shared checkout is not this script's to dirty.
case "$(cd "$EVIDENCE" 2> /dev/null && pwd -P || echo "$EVIDENCE")" in
"$PWD" | "$PWD"/*) die "ITERION_CI_GATE_EVIDENCE points inside the work tree ($EVIDENCE)" ;;
esac
mkdir -p "$EVIDENCE/build"

say "building the bench, CGO_ENABLED=0 — a cgo-linked child cannot confine its own threads"
CGO_ENABLED=0 GOOS=linux GOARCH="${PLATFORM#linux/}" go build -o "$EVIDENCE/build/inpodbench" ./scripts/ci/inpodbench \
	|| die "the bench could not be built, so nothing was tested"
# `go build` honours the umask: a 0700 binary is unexecutable by the pod's uid,
# which the in-pod arm once counted as a mutant being properly refused.
chmod 0755 "$EVIDENCE/build/inpodbench"

# The build context is this directory and nothing else. Building from the repo
# root would ship ~1 GB of context to the daemon for one 14 MB file.
cat > "$EVIDENCE/build/Dockerfile" <<'DOCKERFILE'
ARG BASE
FROM ${BASE}
# The image's own uid differs between the server (10001) and the devbox runner
# (1000); 0755 is what makes the file executable for either, without this layer
# deciding the posture the pod will run under.
COPY inpodbench /bench/inpodbench
DOCKERFILE

TAG="codemode-bench-image:local"
if [ -n "$PUSH_REF" ]; then
	TAG=$PUSH_REF
fi
say "building $TAG from $FROM_IMAGE (platform $PLATFORM)"
docker build \
	--platform "$PLATFORM" \
	--build-arg "BASE=$FROM_IMAGE" \
	--file "$EVIDENCE/build/Dockerfile" \
	--tag "$TAG" \
	"$EVIDENCE/build" > "$EVIDENCE/build.log" 2>&1 \
	|| die "the bench image could not be built: $EVIDENCE/build.log"

cleanup() {
	[ -n "$KEEP" ] && return
	docker rmi "$TAG" > /dev/null 2>&1 || true
}
trap cleanup EXIT

if [ -n "$LOAD_KIND" ]; then
	command -v kind > /dev/null || die "kind is not on PATH, so the witness lane cannot load anything"
	kind load docker-image "$TAG" --name "$LOAD_KIND" > "$EVIDENCE/kind-load.log" 2>&1 \
		|| die "the image could not be loaded into kind cluster '$LOAD_KIND': $EVIDENCE/kind-load.log"
	say "loaded into kind cluster $LOAD_KIND"
	printf 'BENCH_IMAGE=%s\n' "$TAG"
	exit 0
fi

say "pushing $TAG"
docker push "$TAG" > "$EVIDENCE/push.log" 2>&1 \
	|| die "the image could not be pushed: $EVIDENCE/push.log"
# The REPO is the tag minus its tag part — `${TAG%:*}` strips at the LAST colon,
# which is the tag's, not a registry port's. The digest comes from the daemon's
# own RepoDigests, recorded by this push: `docker push` also prints a
# `digest: sha256:…` line, but a caller gluing that onto the repository by hand
# would mint a reference nobody verified.
REPO=${TAG%:*}
case "${TAG##*/}" in
*:*) ;;
*) REPO=$TAG ;;
esac
DIGEST_REF=$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$TAG" 2>/dev/null \
	| grep -F "$REPO@" | head -1)
[ -n "$DIGEST_REF" ] || die "the push succeeded but the daemon recorded no RepoDigest under $REPO, so this run has no pinnable reference: $EVIDENCE/push.log"
say "pushed and pinned"
printf 'BENCH_IMAGE=%s\n' "$DIGEST_REF"
