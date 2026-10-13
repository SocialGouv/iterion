#!/usr/bin/env bash
# Resolves the pinned ultra11y engine and prints the absolute, symlink-resolved
# path of its entry file (scripts/ultra11y.mjs) — or installs it first.
#
# What "pinned" means here, in two layers:
#   - at INSTALL time, `npm ci` from the committed lockfile
#     scripts/ultra11y-engine/package-lock.json: the whole dependency closure
#     (ultra11y and the Playwright/axe packages `scan` needs) is the one
#     recorded there, integrity-checked by npm;
#   - at LOAD time, the entry file must have the pinned size and sha256 from
#     bots/rgaa-audit/ultra11y.pin before anything of it runs, and must then
#     print the pinned version. The fingerprint comes first: an unverified
#     candidate is never executed.
#
# Candidates, in order: $ULTRA11Y_BIN (an explicit choice: refused loudly, never
# silently replaced), the global npm install (the runner image pre-installs
# the engine), then a private install under $ULTRA11Y_HOME/<version>
# (default ~/.cache/iterion/ultra11y). Node is the only tool required.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WANT=$("$ROOT/scripts/ultra11y-pin.sh" version)
SHA=$("$ROOT/scripts/ultra11y-pin.sh" sha256)
SIZE=$("$ROOT/scripts/ultra11y-pin.sh" size)
LOCKDIR="$ROOT/scripts/ultra11y-engine"
export npm_config_update_notifier=false

command -v node >/dev/null 2>&1 || { echo "ultra11y-engine.sh: node is required (>= 22.18)" >&2; exit 1; }

# fingerprint <path> -> "<size> <sha256>" without executing the file.
fingerprint() {
  node -e 'const fs=require("fs"),c=require("crypto");const p=process.argv[1];const b=fs.readFileSync(p);process.stdout.write(b.length+" "+c.createHash("sha256").update(b).digest("hex"))' "$1"
}
realpath_of() { node -e 'process.stdout.write(require("fs").realpathSync(process.argv[1]))' "$1"; }

# ok <path> -> 0 when the file carries the pinned size+sha256 AND prints the
# pinned version. Fingerprint first, run second.
ok() {
  local p=$1 fp got
  [ -f "$p" ] || return 1
  fp=$(fingerprint "$p") || return 1
  [ "$fp" = "$SIZE $SHA" ] || return 1
  got=$(node "$p" --version 2>/dev/null | tail -1) || return 1
  [ "$got" = "$WANT" ]
}

if [ -n "${ULTRA11Y_BIN:-}" ]; then
  if ok "$ULTRA11Y_BIN"; then realpath_of "$ULTRA11Y_BIN"; echo; exit 0; fi
  echo "ultra11y-engine.sh: ULTRA11Y_BIN='$ULTRA11Y_BIN' is not the pinned engine (want version '$WANT', size $SIZE, sha256 $SHA) — refusing to substitute another engine for an explicit choice" >&2
  exit 1
fi

g="$(npm root -g 2>/dev/null || true)"
if [ -n "$g" ] && ok "$g/ultra11y/scripts/ultra11y.mjs"; then realpath_of "$g/ultra11y/scripts/ultra11y.mjs"; echo; exit 0; fi

HOME_DIR=${ULTRA11Y_HOME:-${XDG_CACHE_HOME:-$HOME/.cache}/iterion/ultra11y}
PREFIX="$HOME_DIR/$WANT"
p="$PREFIX/node_modules/ultra11y/scripts/ultra11y.mjs"
if ! ok "$p"; then
  mkdir -p "$PREFIX"
  cp "$LOCKDIR/package.json" "$LOCKDIR/package-lock.json" "$PREFIX/"
  (cd "$PREFIX" && npm ci --ignore-scripts --no-audit --no-fund >&2)
  ok "$p" || { echo "ultra11y-engine.sh: the engine installed from the lockfile at '$p' does not carry the pin (want version '$WANT', size $SIZE, sha256 $SHA)" >&2; exit 1; }
fi
realpath_of "$p"; echo
