#!/usr/bin/env bash
# Fail if any public surface stopped carrying the canonical definition of
# iterion, verbatim.
#
# The sentence lives ONCE, in assets/brand/positioning.md; every surface below
# copies it, because none of them can import from another (a markdown
# frontmatter, a JSON manifest and an HTML <meta> share no mechanism). A copied
# sentence drifts — five different taglines across four surfaces is what this
# repository looked like before this guard — so the copies are checked instead.
#
# Run through `task brand:positioning`; `task check` runs that.
#
# Only the DEFINITION is guarded. The category line ("apps have Linux…") is a
# metaphor each surface phrases in its own voice, and pinning its wording would
# freeze copy that is meant to be written.
set -euo pipefail

cd "$(dirname "$0")/../.."

SOURCE="assets/brand/positioning.md"

# The first fenced line under `## definition`. Anchored on the heading rather
# than on a line number so the prose around it stays editable.
definition="$(
  awk '
    /^## definition$/ { in_section = 1; next }
    /^## / { in_section = 0 }
    in_section && /^```text$/ { getline; print; exit }
  ' "$SOURCE"
)"

if [ -z "$definition" ]; then
  echo "::error::$SOURCE carries no definition — expected a \`\`\`text block under '## definition'"
  exit 1
fi

# Every surface that displays the definition to a reader who is not us.
SURFACES=(
  README.md
  docs/index.md
  docs/.vitepress/config.ts
  docs/scripts/og-card.html
  studio/index.html
  studio/public/manifest.json
  studio/src/views/CloudHome/index.tsx
  charts/iterion/README.md
)

drift=0
for surface in "${SURFACES[@]}"; do
  if [ ! -f "$surface" ]; then
    echo "::error::$surface is on the positioning surface list but does not exist — update $SOURCE and this script together"
    drift=1
    continue
  fi
  # -F: the definition is a literal sentence, and its punctuation must not be
  # read as a pattern.
  if ! grep -qF -- "$definition" "$surface"; then
    echo "::error::positioning drift: $surface no longer carries \"$definition\" — the canonical wording is in $SOURCE"
    drift=1
  fi
done

if [ "$drift" -eq 0 ]; then
  echo "positioning: ${#SURFACES[@]} surfaces carry \"$definition\""
fi

# The OpenGraph image is RENDERED from og-card.html, so a card whose HTML is
# correct can still ship a stale picture. Nothing here can compare pixels
# without a browser; say so rather than imply the image was checked.
echo "positioning: docs/public/og.png is rendered from docs/scripts/og-card.html — run 'task brand:og' after changing the card"

exit $drift
