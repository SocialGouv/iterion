#!/usr/bin/env bash
# Compare the committed baseline with the working chart, including dev values.
set -euo pipefail
base=${1:-HEAD}
root=$(git rev-parse --show-toplevel)
cd "$root"
work=$(mktemp -d "$root/.chart-render-diff.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/before" "$work/after"
git archive "$base" charts/iterion | tar -x -C "$work/before"
cp -R charts "$work/after/"
# Dependencies must already be available locally; this guard never downloads.
# Use the same archives on both sides to isolate the first-party chart diff.
if [ -d charts/iterion/charts ]; then
  cp -R charts/iterion/charts "$work/before/charts/iterion/"
fi
for side in before after; do
  helm template iterion "$work/$side/charts/iterion" \
    --namespace iterion -f "$work/$side/charts/iterion/values-dev.yaml" > "$work/$side.yaml"
done
set +e
diff -u --label "$base (values-dev)" --label 'working tree (values-dev)' "$work/before.yaml" "$work/after.yaml"
status=$?
set -e
if [ "$status" -gt 1 ]; then exit "$status"; fi
if [ "$status" -eq 0 ]; then
  echo 'No rendered change. If a values change was intended, check that the templates consume the key.'
fi
