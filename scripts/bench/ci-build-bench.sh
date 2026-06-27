#!/usr/bin/env sh
# ci-build-bench.sh — factual speed comparison of buildkit-operator vs plain
# `docker buildx` for the iterion container images, across cache states.
#
# It exists because the upstream operator (SocialGouv/buildkit-operator) ships a
# perf *methodology* (docs/performance.md) but no repeatable bench script — this
# is that script, parameterised for the iterion image set and reusable upstream.
#
# Modes:
#   ci-build-bench.sh build [--image NAME] [--runs N] [--out DIR]
#     Times a build across cache states → CSV + Markdown (min/median/max wall-clock
#     + CACHED-step count). States:
#       baseline-cold  plain `docker buildx --no-cache` (fresh)            — worst case, no operator
#       baseline-warm  plain `docker buildx` reusing the local layer cache — best case, no operator
#       operator-cold  buildkit-operator, untrusted=true                   — ephemeral daemon, NO cache (true cold)
#       operator-warm  buildkit-operator, 2nd build same routing key       — warm PVC (cache mounts + layers)
#     operator-* are skipped unless BUILDKIT_OPERATOR_BUILDD_URL + mTLS certs are set.
#
#   ci-build-bench.sh ci-durations [--old-run ID] [--new-run ID]
#     OLD (QEMU multi-arch) vs NEW (operator amd64 + native arm64) full image.yml
#     chain wall-clock, via `gh run view`. No IDs → lists recent main runs to pick from.
#
# Not forced from a pure CI client (need k8s-side BuildProject deletion): S3 cold
# rehydrate (~9x) + daemon cold-start (~19.5s attach). Read them from the operator's
# Prometheus metrics / e2e (TestS3ColdCache). See docs/ci-performance-buildkit-operator.md.
set -eu

IMAGE="bench"; RUNS=3; OUT="${TMPDIR:-/tmp}/iterion-ci-bench"
BKO_REF="${BKO_REF:-20ed87560a925cc4aa3ef453e59aa10dce58975c}"
GATEWAY_HOST="${BUILDKIT_OPERATOR_GATEWAY_HOST:-bkod.fabrique.social.gouv.fr}"
OLD_RUN=""; NEW_RUN=""

mode="${1:-build}"; [ $# -gt 0 ] && shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    --runs)  RUNS="$2";  shift 2 ;;
    --out)   OUT="$2";   shift 2 ;;
    --old-run) OLD_RUN="$2"; shift 2 ;;
    --new-run) NEW_RUN="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
mkdir -p "$OUT"

median() { sort -n | awk '{a[NR]=$1} END{if(NR==0){print "NA";exit} m=int((NR+1)/2); if(NR%2)print a[m]; else printf "%.2f",(a[m]+a[m+1])/2}'; }
minv()   { sort -n | head -n1; }
maxv()   { sort -n | tail -n1; }
now()    { date +%s.%N; }
elapsed(){ awk "BEGIN{printf \"%.2f\", $2-$1}"; }
cached_count() { grep -c 'CACHED' "$1" 2>/dev/null || echo 0; }

# Representative throwaway context (cheap iterations); --image main|slim|full|sec
# points at the real Dockerfiles for production-fidelity numbers.
bench_context() {
  d="$OUT/ctx"; mkdir -p "$d"
  cat > "$d/Dockerfile" <<'DF'
# syntax=docker/dockerfile:1.7
FROM golang:1.26-bookworm
WORKDIR /src
RUN --mount=type=cache,target=/root/.cache/go-build \
    bash -c 'echo "BENCH_RAN=$(date +%s)"; for i in $(seq 1 8); do printf "package p%d\nfunc F%d() int { return %d }\n" "$i" "$i" "$i" > /tmp/p$i.go; go build -o /dev/null /tmp/p$i.go 2>/dev/null || true; done'
DF
  printf '%s' "$d"
}
resolve_target() {
  case "$IMAGE" in
    bench) CTX="$(bench_context)"; FILE="$CTX/Dockerfile"; BUILD_ARGS="" ;;
    main)  CTX="."; FILE="Dockerfile"; BUILD_ARGS="VERSION=bench
COMMIT=bench" ;;
    slim)  CTX="sandbox/slim"; FILE="sandbox/slim/Dockerfile"; BUILD_ARGS="ITERION_IMAGE=ghcr.io/socialgouv/iterion:edge" ;;
    full)  CTX="sandbox/full"; FILE="sandbox/full/Dockerfile"; BUILD_ARGS="BASE=ghcr.io/socialgouv/iterion-sandbox-slim:edge" ;;
    sec)   CTX="sandbox/sec";  FILE="sandbox/sec/Dockerfile";  BUILD_ARGS="BASE=ghcr.io/socialgouv/iterion-sandbox-full:edge" ;;
    *) echo "unknown --image $IMAGE" >&2; exit 2 ;;
  esac
}

baseline() { # $1=label $2=extra flags ; one build, echoes seconds
  log="$OUT/$1.log"; argflags=""
  for a in $BUILD_ARGS; do argflags="$argflags --build-arg $a"; done
  t0="$(now)"
  # shellcheck disable=SC2086
  docker buildx build --builder default $2 $argflags --progress=plain \
    -f "$FILE" --output=type=cacheonly "$CTX" >"$log" 2>&1 || { echo "FAIL"; return 1; }
  t1="$(now)"; elapsed "$t0" "$t1"
}
operator() { # $1=label $2=untrusted $3=routing-name ; one build, echoes seconds
  log="$OUT/$1.log"; bs="$OUT/build.sh"
  t0="$(now)"
  BUILD_ARGS="$BUILD_ARGS" REPO="${GITHUB_REPOSITORY:-socialgouv/iterion}" NAME="$3" ARCH=amd64 \
  BUILD_CONTEXT="$CTX" DOCKERFILE="$FILE" TAGS="iterion-bench:$1" PUSH=false UNTRUSTED="$2" \
  BUILDKIT_OPERATOR_GATEWAY_HOST="$GATEWAY_HOST" \
    sh "$bs" >"$log" 2>&1 || { echo "FAIL"; return 1; }
  t1="$(now)"; elapsed "$t0" "$t1"
}
fetch_build_sh() {
  bs="$OUT/build.sh"
  if [ -n "${BKO_BUILD_SH:-}" ] && [ -f "$BKO_BUILD_SH" ]; then cp "$BKO_BUILD_SH" "$bs";
  else curl -fsSL "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/${BKO_REF}/scripts/build.sh" -o "$bs"; fi
}

run_state() { # $1=label $2=fn $3=arg1 $4=arg2 ; appends a CSV row
  times=""; last=""; i=0
  while [ "$i" -lt "$RUNS" ]; do
    i=$((i+1)); printf '  %-14s run %d/%d... ' "$1" "$i" "$RUNS" >&2
    t="$("$2" "$1" "$3" "$4" 2>>"$OUT/err.log" || true)"
    case "$t" in
      ''|FAIL|*[!0-9.]*) printf 'skip\n' >&2 ;;
      *) times="$times$t
"; last="$OUT/$1.log"; printf '%ss\n' "$t" >&2 ;;
    esac
  done
  if [ -z "$times" ]; then echo "$1,NA,NA,NA,NA,$RUNS" >> "$CSV"; return; fi
  mn="$(printf '%s' "$times" | grep . | minv)"
  md="$(printf '%s' "$times" | grep . | median)"
  mx="$(printf '%s' "$times" | grep . | maxv)"
  echo "$1,$mn,$md,$mx,$(cached_count "$last"),$RUNS" >> "$CSV"
}

do_build() {
  resolve_target
  CSV="$OUT/results-$IMAGE.csv"; MD="$OUT/results-$IMAGE.md"
  echo "state,min_s,median_s,max_s,cached_steps,runs" > "$CSV"
  echo "Benchmarking image=$IMAGE runs=$RUNS → $OUT" >&2

  run_state "baseline-cold" baseline "--no-cache" ""
  run_state "baseline-warm" baseline "" ""

  if [ -n "${BUILDKIT_OPERATOR_BUILDD_URL:-}" ]; then
    fetch_build_sh
    UNIQ="bench-$(od -An -N3 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n' || echo "$$")"
    run_state "operator-cold" operator "true"  "$UNIQ-cold"
    run_state "operator-warm" operator "false" "$UNIQ-warm" # 1st seeds, runs ≥2 are warm
  else
    echo "NOTE: BUILDKIT_OPERATOR_BUILDD_URL unset → operator states skipped (baseline only)." >&2
  fi

  {
    echo "### Build-time benchmark — image \`$IMAGE\` (runs=$RUNS)"; echo
    echo "| cache state | min (s) | median (s) | max (s) | CACHED steps |"
    echo "|---|---|---|---|---|"
    tail -n +2 "$CSV" | while IFS=, read -r st mn md mx cc _; do echo "| $st | $mn | $md | $mx | $cc |"; done
  } > "$MD"
  echo "----"; cat "$MD"; echo; echo "CSV: $CSV"
}

run_minutes() { gh run view "$1" --json createdAt,updatedAt --jq '((.updatedAt|fromdateiso8601)-(.createdAt|fromdateiso8601))/60|(.*10|round)/10'; }
do_ci_durations() {
  command -v gh >/dev/null || { echo "gh not installed" >&2; exit 1; }
  if [ -z "$OLD_RUN" ] || [ -z "$NEW_RUN" ]; then
    echo "Recent successful image.yml runs on main:" >&2
    gh run list --workflow image.yml --branch main --status success -L 6 \
      --json databaseId,headSha,createdAt --jq '.[]|"  run \(.databaseId)  \(.headSha[0:8])  \(.createdAt)"' >&2
    echo "Re-run with --old-run <pre-migration ID> --new-run <post-migration ID>." >&2
    exit 0
  fi
  old="$(run_minutes "$OLD_RUN")"; new="$(run_minutes "$NEW_RUN")"
  sp="$(awk "BEGIN{ if($new>0) printf \"%.2f\", $old/$new; else print \"NA\" }")"
  printf '| image.yml chain | wall-clock (min) |\n|---|---|\n'
  printf '| OLD QEMU multi-arch (run %s) | %s |\n' "$OLD_RUN" "$old"
  printf '| NEW operator amd64 + native arm64 (run %s) | %s |\n' "$NEW_RUN" "$new"
  printf '| speedup | %s× |\n' "$sp"
}

case "$mode" in
  build) do_build ;;
  ci-durations) do_ci_durations ;;
  *) echo "usage: $0 build|ci-durations [flags]" >&2; exit 2 ;;
esac
