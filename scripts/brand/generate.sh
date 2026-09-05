#!/usr/bin/env bash
# Derive every committed copy of the iterion brand assets from the two masters
# in assets/brand/ (the mascot of the official `iterion-bot` GitHub account):
#
#   iterion-bot.png         plain — the account avatar, transparent background
#   iterion-bot-circle.png  badge — dark disc + ring, self-contained on any surface
#
# Run through `task brand:gen` (devbox provides magick, pngquant and oxipng).
# Output is deterministic (no timestamps, fixed quantiser settings), which is
# what lets `task brand:check` regenerate into a temp dir and `cmp` each file:
# a hand-edited copy can not drift from the masters unnoticed.
#
# Usage: generate.sh [OUT_ROOT]   (default: the repo root — writes in place)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${1:-$ROOT}"
PLAIN="$ROOT/assets/brand/iterion-bot.png"
CIRCLE="$ROOT/assets/brand/iterion-bot-circle.png"

# The disc's own navy, sampled from the circle master: the flat background
# behind the badge on surfaces that cannot render alpha (iOS home-screen icons
# turn transparency black).
FLAT_BG="#07121e"

for tool in magick pngquant oxipng; do
  command -v "$tool" >/dev/null || { echo "generate.sh: $tool not on PATH — run through devbox (task brand:gen)" >&2; exit 1; }
done
[ -f "$PLAIN" ] && [ -f "$CIRCLE" ] || { echo "generate.sh: masters missing under assets/brand/" >&2; exit 1; }

WORK="$(mktemp -d -t iterion-brand-XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# The circle master carries a transparent margin around the disc; trimmed once
# so the badge fills every derived square (a 16 px favicon has no pixels to
# waste on padding). The plain master is used as-is: it must stay pixel-equal
# to the avatar the iterion-bot account already wears.
CIRCLE_TRIM="$WORK/circle-trim.png"
magick "$CIRCLE" -trim +repage "$CIRCLE_TRIM"

# compress FILE — lossy palette quantisation (pngquant keeps every alpha
# level) then lossless recompression. pngquant exits 99 when the quality
# floor can't be met; the file is then left unquantised, still recompressed.
compress() {
  pngquant --force --strip --speed 1 --quality 70-98 --output "$1" -- "$1" || [ $? -eq 99 ]
  oxipng -o 4 --strip safe --quiet "$1"
}

# render SRC SIZE OUT — SRC fitted into a SIZE×SIZE transparent square.
render() {
  mkdir -p "$(dirname "$3")"
  magick "$1" -filter Lanczos -resize "$2x$2" -background none -gravity center -extent "$2x$2" \
    -strip -define png:exclude-chunks=date,time "PNG32:$3"
  compress "$3"
}

# render_flat SRC SIZE OUT — same, on the opaque FLAT_BG.
render_flat() {
  mkdir -p "$(dirname "$3")"
  magick "$1" -filter Lanczos -resize "$2x$2" -background "$FLAT_BG" -gravity center -extent "$2x$2" -flatten \
    -strip -define png:exclude-chunks=date,time "PNG24:$3"
  compress "$3"
}

# ---- Go embed (pkg/brand): the avatar uploads + the public /brand/ route ----
render "$PLAIN"        460  "$OUT/pkg/brand/iterion-bot.png"
render "$CIRCLE_TRIM"  512  "$OUT/pkg/brand/iterion-bot-circle.png"

# ---- studio in-app mark (BrandMark), crisp at 4× DPR for a 28–64 px slot ----
render "$CIRCLE_TRIM"  256  "$OUT/studio/src/assets/iterion-mark.png"

# ---- docs site (VitePress nav + hero) and the README banner ----
render "$CIRCLE_TRIM"  512  "$OUT/docs/public/iterion-logo.png"
mkdir -p "$OUT/docs/images" && cp "$OUT/docs/public/iterion-logo.png" "$OUT/docs/images/iterion-logo.png"

# ---- desktop app icon: Wails derives .ico/.icns from this 1024 PNG; the
# release workflow, the .deb/AppImage stage and the Helm chart icon read it too.
# cmd/iterion-desktop/appicon.png is the go:embed copy (build/ is a symlink
# there, which embed cannot follow).
render "$CIRCLE_TRIM" 1024  "$OUT/build/appicon.png"
mkdir -p "$OUT/cmd/iterion-desktop" && cp "$OUT/build/appicon.png" "$OUT/cmd/iterion-desktop/appicon.png"

# ---- favicon pack (studio/public, same file names index.html / manifest.json /
# browserconfig.xml / service-worker.js already reference) ----
PUB="$OUT/studio/public"
for s in 36 48 72 96 144 192; do render "$CIRCLE_TRIM" "$s" "$PUB/android-icon-${s}x${s}.png"; done
for s in 16 32 96;            do render "$CIRCLE_TRIM" "$s" "$PUB/favicon-${s}x${s}.png"; done
for s in 70 144 150 310;      do render "$CIRCLE_TRIM" "$s" "$PUB/ms-icon-${s}x${s}.png"; done
for s in 57 60 72 76 114 120 144 152 180; do render_flat "$CIRCLE_TRIM" "$s" "$PUB/apple-icon-${s}x${s}.png"; done
render_flat "$CIRCLE_TRIM" 192 "$PUB/apple-icon.png"
cp "$PUB/apple-icon.png" "$PUB/apple-icon-precomposed.png"

# Multi-size favicon.ico (16/32/48) for the studio and the docs site.
render "$CIRCLE_TRIM" 48 "$WORK/favicon-48x48.png"
magick "$PUB/favicon-16x16.png" "$PUB/favicon-32x32.png" "$WORK/favicon-48x48.png" "$PUB/favicon.ico"
mkdir -p "$OUT/docs/public" && cp "$PUB/favicon.ico" "$OUT/docs/public/favicon.ico"

echo "brand assets generated under $OUT"
