#!/bin/sh
# Fixed v2 wire; TestImageSmokeFrame pins these bytes to WriteInvocation.
# Run after the final USER in both app and sandbox images. No test installed.
set -eu
[ "$(id -u)" != 0 ] || { echo "execenv smoke must run non-root" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
frame='SUVOVk9ORTIAAAAAAAAAugAAAAAAAAANL2JpbjovdXNyL2JpbgAAAAAAAAAHL2Jpbi9zaAAAAAAAAAABAAAAAAAAAA1FWEVDRU5WX1NNT0tFAAAAAAAAAAhmcmFtZS12MgAAAAAAAAABAAAAAAAAAAQvdG1wAAAAAAAAAAMAAAAAAAAABy9iaW4vc2gAAAAAAAAAAi1jAAAAAAAAACxwcmludGYgJyVzfCVzfCcgIiRFWEVDRU5WX1NNT0tFIiAiJFBXRCI7IGNhdA=='
printf '%s' "$frame" | base64 -d > "$work/input"
printf 'stdin-canary' >> "$work/input"
/usr/local/libexec/iterion-execenv-capsule < "$work/input" > "$work/output" 2> "$work/error"
printf 'IENV-READY-V2\000\000\000frame-v2|/tmp|stdin-canary' > "$work/expected"
cmp "$work/expected" "$work/output"
[ ! -s "$work/error" ]
