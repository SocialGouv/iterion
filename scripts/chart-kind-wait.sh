#!/usr/bin/env sh
# Print the helm --timeout an install of the given values file must watch:
# the file's own server.progressDeadlineSeconds plus a 120 s margin, so the
# deadline — not a fixed client cap — decides whether a slow install is
# called Failed. The kubelet serialises image pulls on a fresh node, so a
# first install can spend many minutes before the server pod even starts
# (measured on kind: 7m15 of pull time for the nats image, iterion#2780);
# a client cap smaller than the deadline makes helm give up while the
# workload is still Progressing.
#
# A values file that leaves the knob unset, or sets it to 0, is refused:
# unset means Kubernetes' 600 s default applies, and a 0 renders through a
# `with` that skips it — either would silently shrink the watch. The first
# `server.progressDeadlineSeconds:` declaration in the file wins.
#
# Whitespace go-yaml — the parser helm reads values with — tolerates (a tab
# separating key and value, spaces or tabs around a trailing comment and
# before the line end, CRLF line endings) is trimmed, so a file helm
# accepts is not refused here as "not an integer" or as unset.
# Usage: scripts/chart-kind-wait.sh <values file>
set -eu

values=${1:?usage: chart-kind-wait.sh <values file>}
[ -f "$values" ] || { echo "chart-kind-wait: no values file at $values" >&2; exit 1; }

# A top-level key closes the previous block: the knob is the one nested
# exactly two spaces under `server:`, never the runner's — values.yaml keeps
# the two side by side at the same indent.
deadline=$(awk '
  /^[A-Za-z][A-Za-z0-9_-]*:/ { top = $1; sub(/\r$/, "", top); sub(/:$/, "", top) }
  top == "server" && $0 ~ /^  progressDeadlineSeconds:/ {
    v = $0
    sub(/^  progressDeadlineSeconds:[ \t]*/, "", v)
    sub(/[ \t]*#.*/, "", v)
    sub(/\r$/, "", v)
    gsub(/["'"'"']/, "", v)
    sub(/[ \t]+$/, "", v)
    print v
    exit
  }
' "$values")

if [ -z "$deadline" ]; then
  echo "chart-kind-wait: $values does not set server.progressDeadlineSeconds — the install would silently watch Kubernetes' default 600 s; set the knob in the values file the install reads" >&2
  exit 1
fi
case $deadline in
  *[!0-9]*)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$deadline' in $values is not an integer" >&2
    exit 1
    ;;
esac
if [ "$deadline" -eq 0 ]; then
  echo "chart-kind-wait: server.progressDeadlineSeconds = 0 in $values renders through a \`with\` that skips it — Kubernetes' 600 s default applies; set a real deadline" >&2
  exit 1
fi

# The deadline budgets the server Deployment alone; the wait also covers the
# stores and the release plumbing, and helm needs a breath after the moment
# the deadline can fire.
margin=120
awk -v d="$deadline" -v m="$margin" 'BEGIN { printf "%ds\n", d + m }'
