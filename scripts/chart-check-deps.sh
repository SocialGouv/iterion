#!/usr/bin/env sh
# Reconcile a chart's sub-chart archives with its Chart.yaml before a render:
# helm renders EVERY archive in <chart>/charts/, declared or not — a stale one
# resurrects the sub-chart it came from (measured: the minio archive still
# rendered 67 minio objects with its dependency removed). An archive is named
# by the Chart.yaml INSIDE it (never by a filename regex), the declared names
# and their status come from `helm dependency list` (no network); undeclared
# archives are removed, and a declared dependency that is missing or at the
# wrong version is fetched with `helm dependency update` — the one network
# step, skipped once the archives are right.
# Usage: scripts/chart-check-deps.sh <chart dir>
set -eu
chart=${1:?usage: chart-check-deps.sh <chart dir>}
[ -f "$chart/Chart.yaml" ] || { echo "chart-check-deps: no Chart.yaml in $chart" >&2; exit 2; }
# NAME VERSION REPOSITORY STATUS…: the status may be several words ("wrong version").
dependency_list() {
  helm dependency list "$chart" | awk 'NR>1 && NF>=4 {status=$4; for (i=5; i<=NF; i++) status=status" "$i; print $1 "\t" status}'
}
declared=$(dependency_list | cut -f1 | tr '\n' ' ')
for archive in "$chart"/charts/*.tgz; do
  [ -e "$archive" ] || continue
  entry=$(tar -tzf "$archive" | grep -E '^[^/]+/Chart\.yaml$' | head -1 || true)
  name=""
  # The chart's own `name:` is the column-0 key, never one nested under
  # maintainers: (the nats chart's first `name:` is "The NATS Authors").
  [ -n "$entry" ] && name=$(tar -xzOf "$archive" "$entry" | awk '/^name:/{gsub(/["'"'"']/,"",$2); print $2; exit}')
  if [ -z "$name" ]; then
    echo "chart-check-deps: removing $archive — not a chart archive (no Chart.yaml inside)"
    rm -v -- "$archive"
    continue
  fi
  case " $declared " in
    *" $name "*) ;;
    *) echo "chart-check-deps: removing $archive — chart '$name' is not declared in Chart.yaml, helm would render it anyway"; rm -v -- "$archive" ;;
  esac
done
# Statuses read AFTER the prune, so an archive this very run removed shows as missing.
needs_update=$(dependency_list | awk -F'\t' '$2 != "ok" {print $1 " (" $2 ")"}' | tr '\n' ' ')
if [ -n "$needs_update" ]; then
  echo "chart-check-deps: dependencies not in place — ${needs_update}— helm dependency update (network)"
  helm dependency update "$chart"
fi
