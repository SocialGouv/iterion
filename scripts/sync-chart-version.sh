#!/usr/bin/env bash
# Sync charts/iterion/Chart.yaml `version` + `appVersion` with package.json
# so OCI consumers can `helm install --version <pkg.json>` and `helm upgrade`
# picks up the right image tag. Used by release-it's after:bump hook (so the
# bump lands in the release commit) and by `task chart:sync-version` (manual
# catch-up).
set -euo pipefail

VERSION=$(node -p "require('./package.json').version")
CHART="charts/iterion/Chart.yaml"

awk -v v="$VERSION" '
  /^version:/    {print "version: " v; next}
  /^appVersion:/ {print "appVersion: \"" v "\""; next}
  {print}
' "$CHART" > "$CHART.tmp"
mv "$CHART.tmp" "$CHART"

echo "Chart.yaml version + appVersion synced to $VERSION"
