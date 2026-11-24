#!/usr/bin/env bash
# Fail if the cloud setup prompt drifted apart across the surfaces that show
# it. The prompt lives in TWO copies that cannot import one another — a TSX
# string on the product landing (the reference: iterion.cloud is where a
# visitor copies it) and the docs walkthrough — so the copies are diffed here,
# the same way brand:positioning holds the definition identical across its
# surfaces.
#
# Run through `task prompt:parity`; `task check` and `check:fast` run that.
set -euo pipefail

cd "$(dirname "$0")/.."

# The needle every copy starts with. Extraction anchors on it rather than on
# a heading or a fence count: a reworded prompt fails LOUDLY here (nothing
# found) instead of silently passing, and an extra fenced block elsewhere in
# a file cannot confuse the read.
NEEDLE='Set up Iterion Cloud (https://iterion.cloud'

# The grammar each extractor reads, named in its refusals. It is kept narrow
# on purpose: a copy written another way is refused loudly with this text,
# rather than the parser growing a rule per spelling.
MD_GRAMMAR='a column-0 ``` fenced block whose lines hold the prompt, the closing fence a bare ``` line'
TSX_GRAMMAR='a line starting with: export const SETUP_PROMPT = ` (template literal, closed by `; at the end of its last line)'

LANDING=studio/src/views/CloudHome/OnePrompt.tsx
WALKTHROUGH=docs/getting-started.md

extract_md() {
  # Reads MD_GRAMMAR only. The WHOLE block that holds the needle is compared,
  # from its opening fence on: a reader copies the block, so a line above the
  # needle is part of what they paste. A second copy in any other form (an
  # indented or ~~~ fence, a blockquote, a <pre>) is caught by the needle
  # count below, which does not depend on this parser.
  awk -v needle="$NEEDLE" '
    # The fences are read the way CommonMark reads them: a fence opened by n
    # backticks closes only on a line of n or more backticks and nothing else;
    # any other backtick-fence line inside, at up to three spaces of
    # indentation, is content. A block holding the
    # prompt must use the grammar exactly — three backticks, no other fence
    # line inside — or it is refused: the renderer would extend it past where
    # a simpler reading stops, and the parity would compare the wrong text.
    !infence && /^```/ {
      match($0, /^`+/); n = RLENGTH
      infence = 1; buf = ""; has = 0; odd = 0; next
    }
    infence && /^`+[ \t]*$/ {
      match($0, /^`+/)
      if (RLENGTH >= n) {
        infence = 0
        if (has) {
          if (n > 3 || odd) { print "the fenced block carrying the setup prompt is outside the grammar this check reads: three backticks, no other fence line inside" > "/dev/stderr"; failed = 1; exit 3 }
          if (found) { print "a second fenced block carries the setup prompt" > "/dev/stderr"; failed = 1; exit 3 }
          found = 1; out = buf
        }
        next
      }
    }
    infence {
      if ($0 ~ /^ ? ? ?```/) odd = 1
      line = $0; sub(/[ \t]+$/, "", line); buf = buf line "\n"
      if (index($0, needle)) has = 1
    }
    END {
      if (failed) exit 3
      if (infence && has) { print "the fenced block carrying the setup prompt never closes" > "/dev/stderr"; exit 3 }
      printf "%s", out
    }
  ' "$1"
}

extract_tsx() {
  # The text compared is what the visitor copies, so the literal is read the
  # way JavaScript renders it. The only escape rendered here is \` (a
  # backtick); any other backslash, a ${…} interpolation or a bare backtick
  # before the closing one renders differently from its spelling, and is
  # refused rather than compared as written. A second declaration is refused
  # too: only one of them is the prompt the page shows.
  awk '
    /^export const SETUP_PROMPT = `/ {
      if (seen) { print "a second SETUP_PROMPT declaration" > "/dev/stderr"; failed = 1; exit 3 }
      seen = 1; inblock = 1
      sub(/^export const SETUP_PROMPT = `/, "")
    }
    inblock {
      line = $0
      closed = (line ~ /(^|[^\\])`;[ \t]*$/)
      if (closed) sub(/`;[ \t]*$/, "", line)
      rest = line; gsub(/\\`/, "", rest)
      if (index(rest, "\\") || index(rest, "`") || index(rest, "${")) {
        print "SETUP_PROMPT carries an escape, an interpolation or a backtick this check cannot render: " line > "/dev/stderr"
        failed = 1; exit 3
      }
      gsub(/\\`/, "`", line)
      sub(/[ \t]+$/, "", line)
      print line
      if (closed) inblock = 0
    }
    END {
      if (failed) exit 3
      if (inblock) { print "SETUP_PROMPT never closes" > "/dev/stderr"; exit 3 }
    }
  ' "$1"
}

read_copy() {
  local file="$1" extractor="$2" grammar="$3"
  local body
  # The status is checked here, explicitly: this function runs inside the
  # caller's command substitution, where bash does not apply `set -e`, so an
  # extractor that refuses its input would otherwise pass on whatever it
  # printed before refusing.
  if ! body="$("$extractor" "$file")"; then
    echo "::error::$file: the setup prompt could not be read (see the line above)" >&2
    exit 1
  fi
  if [ -z "$body" ]; then
    # stderr, not stdout: this echo is captured by the caller's command
    # substitution, and a stdout line would vanish into `$reference` while
    # `set -e` kills the script with no output at all — the exact trap
    # scripts/brand/positioning-check.sh documents and neutralises.
    echo "::error::$file carries no setup prompt this check can read — expected $grammar, starting with: $NEEDLE" >&2
    exit 1
  fi
  printf '%s\n' "$body"
}

reference="$(read_copy "$LANDING" extract_tsx "$TSX_GRAMMAR")"
walkthrough="$(read_copy "$WALKTHROUGH" extract_md "$MD_GRAMMAR")"

# One copy per surface, whatever its markup: the walkthrough carries the
# needle on exactly one line. A stale second copy in a form the extractor does
# not parse would otherwise sit there uncompared; prose that needs to cite the
# prompt paraphrases its first line rather than quoting it.
copies="$(grep -cF -- "$NEEDLE" "$WALKTHROUGH")" || copies=0
if [ "$copies" -ne 1 ]; then
  echo "::error::$WALKTHROUGH carries the setup prompt's first line on $copies lines — exactly one copy, the compared block, may carry it"
  exit 1
fi

if ! diff <(printf '%s\n' "$reference") <(printf '%s\n' "$walkthrough") >/dev/null; then
  # Two named files, no blame: a drift in EITHER file produces this failure,
  # and the message must not pre-judge which one moved.
  echo "::error::setup-prompt drift: $LANDING and $WALKTHROUGH disagree — one of them drifted; the two copies must stay identical; 'task prompt:parity' re-checks after your edit."
  exit 1
fi

echo "prompt parity: 2/2 copies identical"
