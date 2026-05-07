#!/usr/bin/env bash
# Sync helm/iterion/Chart.yaml appVersion with package.json so
# `helm upgrade` picks up the right image tag. Used by release-it's
# after:bump hook (so the bump lands in the release commit) and by
# `task chart:sync-version` (manual catch-up).
set -euo pipefail

VERSION=$(node -p "require('./package.json').version")
CHART="helm/iterion/Chart.yaml"

awk -v v="$VERSION" '/^appVersion:/ {print "appVersion: \"" v "\""; next} {print}' "$CHART" > "$CHART.tmp"
mv "$CHART.tmp" "$CHART"

echo "Chart.yaml appVersion synced to $VERSION"
