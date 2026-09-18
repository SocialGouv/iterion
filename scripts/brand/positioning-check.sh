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
# Run through `task brand:positioning`; `task check` and the CI `brand` job
# both run that.
#
# Only the DEFINITION is guarded. The category line ("apps have Linux…") is a
# metaphor each surface phrases in its own voice, and pinning its wording would
# freeze copy that is meant to be written. The SHORT form used by packaging
# metadata is out of scope too, and assets/brand/positioning.md says which
# surfaces those are — the list below is not the whole class, and claiming
# otherwise is how a guard starts lying.
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

# Every surface that displays the definition to a reader who is not us, with
# the number of times it must appear.
#
# The COUNT is the guard, not mere presence. Two of these carry the sentence
# three times over (a description plus an OpenGraph and a Twitter card), and a
# presence test is satisfied by any one survivor: blanking the two social
# cards — the exact regression this guard exists to catch, since a link preview
# is the whole point of them — left `grep -q` green.
#
# A mismatch in EITHER direction is drift: a lost copy, and equally a new
# unlisted one (a sentence added in a comment while the rendered copy is
# deleted nets to a count that no longer matches).
SURFACES=(
  "README.md:1"
  "charts/iterion/README.md:1"
  "docs/cloud-overview.md:1"
  "docs/index.md:1"
  "docs/scripts/og-card.html:1"
  "docs/.vitepress/config.ts:3"
  "studio/index.html:3"
  "studio/public/manifest.json:1"
  "studio/src/views/CloudHome/index.tsx:1"
)

drift=0
for entry in "${SURFACES[@]}"; do
  surface="${entry%:*}"
  want="${entry##*:}"
  if [ ! -f "$surface" ]; then
    echo "::error::$surface is on the positioning surface list but does not exist — update $SOURCE and this script together"
    drift=1
    continue
  fi
  # Newlines folded to spaces before matching: on a prose surface the sentence
  # heads a hard-wrapped paragraph, and a line-oriented match turns an ordinary
  # editorial reflow into "the sentence is gone" — a false accusation in a
  # guard that blocks the build. -F because the definition is a literal
  # sentence whose punctuation must not be read as a pattern.
  # `|| got=0` is load-bearing, not defensive. grep exits 1 when it matches
  # nothing, pipefail propagates that, and a bare assignment from a command
  # substitution then aborts the script under `set -e` — so the one case this
  # guard exists for, a surface that lost its last copy, exited 1 with NO
  # OUTPUT AT ALL: no ::error::, no surface named, and every later surface
  # unchecked. Measured on a bare container before this line existed.
  got="$(tr '\n' ' ' < "$surface" | tr -s ' ' | grep -oF -- "$definition" | wc -l)" || got=0
  if [ "$got" != "$want" ]; then
    echo "::error::positioning drift: $surface carries the definition $got time(s), expected $want — the canonical wording is in $SOURCE"
    drift=1
  fi
done

if [ "$drift" -eq 0 ]; then
  echo "positioning: ${#SURFACES[@]} surfaces carry \"$definition\" the expected number of times"
fi

exit $drift
