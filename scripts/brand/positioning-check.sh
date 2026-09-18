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
# Two things are guarded, and one deliberately is not:
#
#   - the DEFINITION, on the surfaces that display a full sentence;
#   - the SHORT form (same sentence, no final period) on packaging metadata
#     whose format refuses one — a Homebrew `desc` audit rejects a trailing
#     period and a leading article, a `.desktop` Comment= is a one-liner;
#   - NOT the category line ("apps have Linux…"): it is a metaphor each
#     surface phrases in its own voice, and pinning its wording would freeze
#     copy that is meant to be written.
#
# Both lists are GUARDED rather than merely written down, because the
# hand-written inventory has been wrong at every attempt: a grep for the old
# tagline missed CLAUDE.md's opening line, then a narrower grep for one PHRASE
# missed four more — including the `GenericName=` on the line directly above a
# `Comment=` the same change had just edited. A count in a comment goes stale;
# the lists below are what the build reads.
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

# Normalised the same way the surfaces are before matching. Without this a
# definition carrying a double space — or a trailing one, which also defeats
# ${definition%.} — turns every surface red at once, with no edit to any of
# them that could fix it: the needle would be the only thing wrong, and the
# error names the haystack.
definition="$(printf '%s' "$definition" | tr -s ' ' | sed 's/^ *//; s/ *$//')"

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

# The SHORT form: the same sentence without its final period, on metadata whose
# format refuses a sentence. Presence, not a count — these carry it once by
# construction, and several embed it mid-phrase.
SHORT_SURFACES=(
  "CLAUDE.md"
  "Cask/iterion-desktop.rb"
  "Formula/iterion.rb"
  "build/linux/iterion.desktop"
  "build/windows/info.json"
  "charts/iterion/Chart.yaml"
  "cmd/iterion-desktop/wails.json"
  "cmd/iterion/main.go"
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

# The short form is the definition minus its final period, derived rather than
# repeated: a second literal here is a second thing to keep in step.
short="${definition%.}"
for surface in "${SHORT_SURFACES[@]}"; do
  if [ ! -f "$surface" ]; then
    echo "::error::$surface is on the short-form surface list but does not exist — update $SOURCE and this script together"
    drift=1
    continue
  fi
  folded="$(tr '\n' ' ' < "$surface" | tr -s ' ')"
  if ! printf '%s' "$folded" | grep -qF -- "$short"; then
    echo "::error::positioning drift: $surface no longer carries the short form \"$short\" — the canonical wording is in $SOURCE"
    drift=1
  elif printf '%s' "$folded" | grep -qF -- "$definition"; then
    # The short form is a SUBSTRING of the definition, so a presence test alone
    # is satisfied by the full sentence — and these surfaces exist precisely
    # because the full sentence is refused there. Measured: a Homebrew `desc`
    # set to the definition passed this guard green while `brew style` rejected
    # it with "Description shouldn't end with a full stop."
    echo "::error::positioning drift: $surface carries the full definition where the SHORT form belongs — its format refuses a final period (see $SOURCE)"
    drift=1
  fi
done

if [ "$drift" -eq 0 ]; then
  echo "positioning: ${#SURFACES[@]} surfaces carry \"$definition\", ${#SHORT_SURFACES[@]} carry the short form"
fi

exit $drift
