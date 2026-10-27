#!/usr/bin/env bash
# ADR-138 metric: third-party source linked into the server binary.
# Prints the linked package count, the non-stdlib LOC total and the top
# contributors. Run via `task deps:closure`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
out="$(mktemp "${TMPDIR:-/tmp}/deps-closure.XXXXXX")"
trap 'rm -f "$out" "$out".pkgs "$out".dirs "$out".loc' EXIT

go list -deps ./cmd/iterion > "$out.pkgs"
printf 'packages linked: %s\n' "$(wc -l < "$out.pkgs")"

go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/iterion | awk 'NF' > "$out.dirs.raw"
# the module's own packages are non-stdlib too: keep the metric third-party only
# (vendor mode: every .Dir lives under the repo root, so filter by subtree)
top="$(git rev-parse --show-toplevel)"
grep -E "^($top/vendor/|$top/third_party/)" "$out.dirs.raw" > "$out.dirs" || true
printf 'non-stdlib packages: %s\n' "$(wc -l < "$out.dirs")"

: > "$out.loc"
while read -r d; do
  loc=$(find "$d" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -print0 2>/dev/null | xargs -0 -r cat 2>/dev/null | wc -l)
  printf '%s %s\n' "$loc" "$d" >> "$out.loc"
done < "$out.dirs"

sort -rn "$out.loc" | sed -n '1,15p'
printf 'TOTAL third-party linked LOC: %s\n' "$(awk '{s+=$1} END{print s}' "$out.loc")"
