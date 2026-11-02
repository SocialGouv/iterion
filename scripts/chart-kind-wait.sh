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
# The knob's value is resolved the way go-yaml — the parser helm reads
# values with — resolves it, so a file helm accepts is not refused here as
# "not an integer": an optional sign and `_` separators are cosmetic,
# 0x/0b/0o and a leading-0 form carry their radix (0600 is octal 384, not
# 600), and a float that is integral lands as its integer (600.0 renders
# as 600). A value go-yaml hands over as a string — a word, base-60 like
# 1:30, a non-integral float — helm renders verbatim and the API refuses,
# so it is refused here too. A negative deadline is refused as well: it
# cannot budget the watch.
#
# Whitespace go-yaml tolerates (a tab separating key and value, spaces or
# tabs around a trailing comment and before the line end, CRLF line
# endings) is trimmed, so such a file is not refused either.
# Usage: scripts/chart-kind-wait.sh <values file>
set -eu

values=${1:?usage: chart-kind-wait.sh <values file>}
[ -f "$values" ] || { echo "chart-kind-wait: no values file at $values" >&2; exit 1; }

# Any column-0 key closes the previous block — whatever it starts with:
# go-yaml accepts `_helpers:` or `2nd:` as top-level keys, and a block
# nested under one must not keep the previous top key. The knob is the one
# nested exactly two spaces under `server:`, never the runner's — values.yaml
# keeps the two side by side at the same indent.
raw=$(awk '
  /^[^ \t\r#]/ { top = $1; sub(/\r$/, "", top); sub(/:$/, "", top) }
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

if [ -z "$raw" ]; then
  echo "chart-kind-wait: $values does not set server.progressDeadlineSeconds — the install would silently watch Kubernetes' default 600 s; set the knob in the values file the install reads" >&2
  exit 1
fi

# Resolve the raw scalar the way go-yaml — the parser helm reads values
# with — hands it to the templates. Branch order mirrors go-yaml's own
# resolution: radix prefixes first, then a decimal that must not start
# with 0 (go-yaml reads those as octal or, on a digit outside [0-7],
# falls through to the float form), then the float form go-yaml resolves
# when the integer reads fail — 08 is 8. A value outside the exact awk
# integer range is refused: the watch is printed through awk doubles,
# which would round it and print a wait the file never set.
rc=0
deadline=$(printf '%s\n' "$raw" | awk '{
  s = $0
  neg = 0
  if (s ~ /^[+-]/) { neg = (substr(s, 1, 1) == "-"); s = substr(s, 2) }
  gsub(/_/, "", s)
  v = 0
  if (s ~ /^0[xX][0-9a-fA-F]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 16 + index("0123456789ABCDEF", toupper(substr(s, i, 1))) - 1
  } else if (s ~ /^0[bB][0-1]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 2 + (substr(s, i, 1) == "1")
  } else if (s ~ /^0[oO][0-7]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 8 + (substr(s, i, 1) + 0)
  } else if (s ~ /^0[0-7]+$/) {
    for (i = 2; i <= length(s); i++)
      v = v * 8 + (substr(s, i, 1) + 0)
  } else if (s ~ /^(0|[1-9][0-9]*)$/) {
    v = s + 0
  } else if (s ~ /^(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][+-]?[0-9]+)?$/) {
    v = s + 0
    if (v != int(v)) exit 3
  } else {
    exit 3
  }
  if (v >= 9007199254740992) exit 4
  if (v == 0) neg = 0
  print (neg ? -v : v)
}') || rc=$?
case $rc in
  0) ;;
  3)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is not an integer — go-yaml hands it to the templates verbatim and the API refuses the Deployment" >&2
    exit 1
    ;;
  4)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is beyond the exact integer range this guard resolves — the printed wait would not be the deadline the file sets" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: could not resolve server.progressDeadlineSeconds = '$raw' in $values (awk exited $rc)" >&2
    exit 1
    ;;
esac
case $deadline in
  -*)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is negative — a negative deadline cannot budget the install watch; set a real deadline" >&2
    exit 1
    ;;
esac
if [ "$deadline" -eq 0 ]; then
  echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values renders through a \`with\` that skips it — Kubernetes' 600 s default applies; set a real deadline" >&2
  exit 1
fi

# The deadline budgets the server Deployment alone; the wait also covers the
# stores and the release plumbing, and helm needs a breath after the moment
# the deadline can fire.
margin=120
awk -v d="$deadline" -v m="$margin" 'BEGIN { printf "%ds\n", d + m }'
