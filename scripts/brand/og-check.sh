#!/usr/bin/env bash
# Fail if docs/public/og.png is not a current, well-formed render of
# docs/scripts/og-card.html.
#
# Run through `task brand:og:check`; `task check` and the CI `brand` job both
# run it. One script rather than a copy in each, because the first version of
# this guard lived as shell duplicated into the Taskfile and the workflow, and
# two copies of a hash command is two places for the next change to land in one.
#
# TWO things are checked, because each alone has been the hole:
#
#   1. THE SOURCE — the recorded hash in docs/public/og.png.sha must match
#      og-card.html. A pixel comparison against a fresh render was the obvious
#      guard and is the wrong one: the card asks for "Noto Sans", nothing in
#      devbox.json pins a font, and chromium falls back to whatever the host
#      offers, so the same card renders to different bytes on a devcontainer,
#      on macOS or on a bare CI image. In `task check` that is a blocking tool
#      telling a colleague to commit a regenerated PNG that then reddens for
#      the next person.
#
#   2. THE ARTEFACT — og.png must exist, be a PNG, and be 1200×630. The
#      source-hash check alone never opens the file it is named for: measured,
#      it reported "matches the committed og-card.html" with og.png DELETED.
#      Dimensions come from the IHDR header, so they are font-independent —
#      this says nothing about what the card LOOKS like, and does not pretend
#      to. What keeps the layout legible is the card surviving the widest font
#      in its own stack; og-card.html carries that constraint at its `h1`.
set -euo pipefail

cd "$(dirname "$0")/../.."

CARD="docs/scripts/og-card.html"
PNG="docs/public/og.png"
SHA="docs/public/og.png.sha"

# sha256sum is GNU; macOS ships shasum instead. Same two-way form the repo
# already uses in scripts/desktop/generate-manifest.sh and docs/install.sh.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

if [ ! -f "$PNG" ]; then
  echo "::error::$PNG is missing — run 'task brand:og' and commit the result"
  exit 1
fi

recorded="$(cat "$SHA" 2>/dev/null || true)"
if [ -z "$recorded" ]; then
  echo "::error::$SHA is missing or empty — run 'task brand:og' and commit both files"
  exit 1
fi

if [ "$recorded" != "$(sha256_of "$CARD")" ]; then
  echo "::error::$PNG was rendered from a different $CARD — run 'task brand:og' and commit the result"
  exit 1
fi

# The PNG magic plus the IHDR width/height, big-endian at bytes 16..23.
dims="$(python3 - "$PNG" <<'PY'
import struct, sys
data = open(sys.argv[1], "rb").read(24)
if len(data) < 24 or data[:8] != b"\x89PNG\r\n\x1a\n":
    print("notpng")
else:
    print("%dx%d" % struct.unpack(">II", data[16:24]))
PY
)"
if [ "$dims" != "1200x630" ]; then
  echo "::error::$PNG is $dims, expected a 1200x630 PNG — run 'task brand:og' and commit the result"
  exit 1
fi

echo "positioning: $PNG is a 1200x630 render of the committed $CARD"
