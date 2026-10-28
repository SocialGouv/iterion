#!/usr/bin/env sh
# Reconcile a chart's sub-chart archives with its Chart.yaml before a render:
# helm renders EVERY archive in <chart>/charts/, declared or not — a stale one
# resurrects the sub-chart it came from (measured: the minio archive still
# rendered 67 minio objects with its dependency removed). An archive is named
# by the Chart.yaml INSIDE it (never by a filename regex), the declared names
# and their status come from `helm dependency list` (no network); undeclared
# archives are removed, and a declared dependency that is missing or at the
# wrong version is fetched with `helm dependency update` — the one network
# step, skipped once the archives are right. The one thing this check must
# never do is remove a DECLARED archive: a listing it could not read is a
# failure of the check, never an empty list.
# Usage: scripts/chart-check-deps.sh <chart dir>
set -eu
chart=${1:?usage: chart-check-deps.sh <chart dir>}
[ -f "$chart/Chart.yaml" ] || { echo "chart-check-deps: no Chart.yaml in $chart" >&2; exit 2; }
command -v helm >/dev/null 2>&1 || { echo "chart-check-deps: helm is not on PATH (run under devbox)" >&2; exit 2; }
# NAME, VERSION, REPOSITORY and STATUS are tab-separated and space-padded; an
# empty REPOSITORY (a local dependency) is an empty cell, kept by -F'\t'.
# helm's own WARNING lines (an unpacked directory not in Chart.yaml) are not
# rows. A failing `helm dependency list` (a corrupt archive next to the
# declared ones, helm itself broken) fails the check with its message.
dependency_list() {
  out=$(helm dependency list "$chart") || { echo "chart-check-deps: helm dependency list failed — nothing was removed:" >&2; printf '%s\n' "$out" >&2; return 1; }
  # The header is skipped by content, not by line number: helm prints its
  # WARNING lines before it.
  printf '%s\n' "$out" | tr -d '\r' | awk -F'\t' 'NF>=4 && $1 !~ /^WARNING/ {name=$1; status=$4; gsub(/^ +| +$/, "", name); gsub(/^ +| +$/, "", status); if (name != "" && name != "NAME") print name "\t" status}'
}
listing=$(dependency_list) || exit 1
declared=$(printf '%s\n' "$listing" | cut -f1 | tr '\n' ' ')
# An unpacked sub-chart DIRECTORY is rendered by helm too; this check removes
# no directory — it refuses, and the operator decides.
for dir in "$chart"/charts/*/; do
  [ -d "$dir" ] || continue
  echo "chart-check-deps: $dir is an unpacked sub-chart directory — helm renders it; remove or pack it by hand (this check never removes a directory)" >&2
  exit 1
done
for archive in "$chart"/charts/*.tgz; do
  [ -e "$archive" ] || continue
  entry=$(tar -tzf "$archive" 2>/dev/null | grep -E '^[^/]+/Chart\.yaml$' | head -1 || true)
  name=""
  # The chart's own `name:` is the column-0 key, never one nested under
  # maintainers: (the nats chart's first `name:` is "The NATS Authors").
  [ -n "$entry" ] && name=$(tar -xzOf "$archive" "$entry" | tr -d '\r' | awk '/^name:/{gsub(/["'"'"']/,"",$2); print $2; exit}')
  if [ -z "$name" ]; then
    echo "chart-check-deps: removing $archive — not a chart archive (no readable Chart.yaml inside)"
    rm -v -- "$archive"
    continue
  fi
  case " $declared " in
    *" $name "*) ;;
    *) echo "chart-check-deps: removing $archive — chart '$name' is not declared in Chart.yaml, helm would render it anyway"; rm -v -- "$archive" ;;
  esac
done
# Statuses read AFTER the prune, so an archive this very run removed shows as missing.
listing=$(dependency_list) || exit 1
needs_update=$(printf '%s\n' "$listing" | awk -F'\t' '$2 != "ok" {print $1 " (" $2 ")"}' | tr '\n' ' ')
if [ -n "$needs_update" ]; then
  echo "chart-check-deps: dependencies not in place — ${needs_update}— helm dependency update (network)"
  helm dependency update "$chart"
fi
