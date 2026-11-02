#!/usr/bin/env bash
# Fail if the cloud setup prompt drifted apart across the surfaces that show
# it. The prompt lives in FOUR copies that cannot import one another — the
# README, two markdown pages of the docs site, and a TSX string on the
# product landing — so the copies are diffed here, the same way
# brand:positioning holds the definition identical across its surfaces.
#
# Run through `task prompt:parity`; `task check` and `check:fast` run that.
set -euo pipefail

cd "$(dirname "$0")/.."

# The needle every copy starts with. Extraction anchors on it rather than on
# a heading or a fence count: a reworded prompt fails LOUDLY here (nothing
# found) instead of silently passing, and an extra fenced block elsewhere in
# a file cannot confuse the read.
NEEDLE='Set up Iterion Cloud (https://iterion.cloud'

extract_md() {
  # Fence-aware: the needle matches only INSIDE a fenced block, so prose
  # quoting the prompt's first line ahead of the block cannot hijack the
  # extraction into a false drift.
  awk -v needle="$NEEDLE" '
    /^```/ { infence = !infence }
    infence && index($0, needle) { inblock = 1 }
    inblock && /^```$/ { exit }
    inblock { sub(/[ \t]+$/, ""); print }
  ' "$1"
}

extract_tsx() {
  # One rule does everything, terminal line included: the template's last
  # content line also carries the closing backtick, and a second rule would
  # print it without the unescape below. The literal escapes its backticks
  # (\`): unescape them so the text compared here is what the visitor copies,
  # not the source spelling.
  awk '
    /export const SETUP_PROMPT = `Set up Iterion Cloud/ {
      inblock = 1
      sub(/^.*export const SETUP_PROMPT = `/, "")
    }
    inblock {
      sub(/[ \t]+$/, "")
      closed = sub(/[ \t]*`;$/, "")
      gsub(/\\`/, "`")
      print
      if (closed) exit
    }
  ' studio/src/views/CloudHome/OnePrompt.tsx
}

read_copy() {
  local name="$1" file="$2" extractor="$3"
  local body
  body="$("$extractor" "$file")"
  if [ -z "$body" ]; then
    # stderr, not stdout: this echo is captured by the caller's command
    # substitution, and a stdout line would vanish into `$reference` while
    # `set -e` kills the script with no output at all — the exact trap
    # scripts/brand/positioning-check.sh documents and neutralises.
    echo "::error::$name carries no setup prompt — expected a block starting with: $NEEDLE" >&2
    exit 1
  fi
  printf '%s\n' "$body"
}

reference="$(read_copy "README.md" README.md extract_md)"

fail() {
  # Two named files, no blame: the star topology diffs every copy against
  # README.md, so a drift in EITHER file produces this failure and the
  # message must not pre-judge which one moved.
  echo "::error::setup-prompt drift: README.md and $1 disagree — one of them drifted; the four copies (README.md, docs/getting-started.md, docs/index.md, studio/src/views/CloudHome/OnePrompt.tsx) must stay identical; 'task prompt:parity' re-checks after your edit."
  exit 1
}

diff <(printf '%s\n' "$reference") <(read_copy "docs/getting-started.md" docs/getting-started.md extract_md) >/dev/null || fail "docs/getting-started.md"
diff <(printf '%s\n' "$reference") <(read_copy "docs/index.md" docs/index.md extract_md) >/dev/null || fail "docs/index.md"
diff <(printf '%s\n' "$reference") <(read_copy "studio/src/views/CloudHome/OnePrompt.tsx" studio/src/views/CloudHome/OnePrompt.tsx extract_tsx) >/dev/null || fail "studio/src/views/CloudHome/OnePrompt.tsx"

echo "prompt parity: 4/4 copies identical"
