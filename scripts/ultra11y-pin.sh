#!/usr/bin/env bash
# Prints the pinned ultra11y engine version (default), or one field of the pin
# file: `ultra11y-pin.sh [version|sha256|size|integrity]`. The pin lives in
# bots/rgaa-audit/ultra11y.pin — the single grep point for CI, the runner image
# and the Taskfile.
set -euo pipefail
PIN="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bots/rgaa-audit/ultra11y.pin"
field=${1:-version}
case "$field" in version|sha256|size|integrity) ;; *) echo "usage: $0 [version|sha256|size|integrity]" >&2; exit 2 ;; esac
value=$(sed -n "s/^${field}=//p" "$PIN" | head -1)
[ -n "$value" ] || { echo "ultra11y-pin.sh: no '$field' in $PIN" >&2; exit 1; }
printf '%s\n' "$value"
