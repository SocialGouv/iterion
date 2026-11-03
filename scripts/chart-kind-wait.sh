#!/usr/bin/env sh
# Print the helm --timeout an install of the given values file must watch:
# the file's own server.progressDeadlineSeconds plus a 120 s margin, so the
# deadline — not a fixed client cap — decides whether a slow install is
# called Failed. The kubelet serialises image pulls on a fresh node, so a
# first install can spend many minutes before the server pod even starts
# (measured on kind: 7m15 of pull time for the nats image, iterion#2780);
# a client cap smaller than the deadline makes helm give up while the
# workload is still Progressing.
#
# A values file that leaves the knob unset, or sets it to 0, is refused:
# unset means Kubernetes' 600 s default applies, and a 0 renders through a
# `with` that skips it — either would silently shrink the watch. The knob
# is the server.progressDeadlineSeconds declaration the install runs
# with, and go-yaml — the parser helm reads values with — scopes which
# one that is (both rules measured on helm 4.1.1): inside one document a
# repeated `server:` block replaces the whole map, so a knob declared
# only in the earlier block renders no field and Kubernetes' 600 s
# default applies, while helm merges `---` documents per key, so a knob
# from an earlier document survives a later one that does not repeat it.
# The reader drops the knob when a column-0 `server:` line re-opens the
# block in the same document and keeps it across a `---` boundary.
#
# The extractor reads the plain block form: the knob nested under
# `server:` at whatever uniform indent the block's keys use — the block's
# first indented key fixes that indent, and a deeper key belongs to a
# nested block, not to the knob — with the top key and the knob key bare
# or quoted, an optional space before the colon, and the value on the
# same line. What a line reader cannot resolve is refused loudly, never
# guessed: anchors and merge keys, a flow mapping or sequence that stays
# open past its opening line, and a scalar on the line after the key
# stay invisible to it, and such a file is refused rather than sized
# from a value helm does not use. go-yaml ignores indentation inside
# flow, so a line reader cannot tell which key owns a later line — a
# flow interior read as a column-0 key re-anchors the reader and a knob
# one line down is read from a map it does not belong to (helm 4.1.1
# renders that file clean with no server deadline, and Kubernetes' 600 s
# default applies at install) — the file is refused at the line that
# opens the flow. The indented content of a literal or folded block
# scalar is raw text to go-yaml — no line of it is a key — so the flow
# scan skips those lines, and a `{` or `[` opening one is a character of
# the string, not a flow collection (helm 4.1.1 installs such a file); a
# line no deeper than the opener ends the scalar and is read like the
# rest of the file. Braces quoted or commented away open nothing, and
# the braces of a plain value are the value's own (`a:{b`,
# `https://…/{id}` are one scalar to go-yaml), so neither is refused as
# flow. (The
# merge-key route cannot reach an install either way: the chart's values
# schema rejects the top-level anchor holder it needs.)
#
# The knob's value is resolved the way go-yaml — the parser helm reads
# values with — resolves it, so a file helm accepts is not refused here as
# "not an integer": an optional sign and `_` separators are cosmetic,
# 0x/0b/0o and a leading-0 form carry their radix (0600 is octal 384, not
# 600), and a float that is integral lands as its integer (600.0 renders
# as 600). A value go-yaml hands over as a string — a word, base-60 like
# 1:30, a non-integral float — helm renders verbatim and the API refuses,
# so it is refused here too. A quoted decimal is the one string form that
# installs anyway: the templates print the scalar verbatim and the applied
# manifest reads it back as the integer it spells (helm 4.1.1 renders
# "600" byte-identical to 600), so it is accepted here. Only one pair of
# enclosing quotes is that form: a quote inside a plain scalar is a
# character of it (`6"00` is the string it spells), and a single-quoted
# `'"600"'` keeps its inner quotes through the render, so both are
# refused as the strings go-yaml hands over. A negative deadline is
# refused as well: it cannot budget the watch.
#
# A `#` separated from the value by whitespace is a comment, and the
# whitespace go-yaml tolerates around one (a tab separating key and
# value, spaces or tabs before the line end, CRLF line endings) is
# trimmed, so such a file is not refused. A `#` glued to the value is
# part of it: go-yaml hands `600#x` over as the string it spells — helm
# renders it verbatim and the API refuses the Deployment — so it is
# refused here as well, not sized from its leading digits. A value that
# resolves to one starting with `#` is no scalar at all:
# `progressDeadlineSeconds: # note` is null to go-yaml, and a quoted
# `"#x"` renders as `progressDeadlineSeconds: #x`, where the whitespace
# before the `#` makes the applied manifest read the field as empty —
# Kubernetes' 600 s default applies either way, so both route to the
# unset refusal instead of claiming an API refusal helm never sees.
# Usage: scripts/chart-kind-wait.sh <values file>
set -eu

values=${1:?usage: chart-kind-wait.sh <values file>}
[ -f "$values" ] || { echo "chart-kind-wait: no values file at $values" >&2; exit 1; }

# Any column-0 key closes the previous block — whatever it starts with:
# go-yaml accepts `_helpers:` or `2nd:` as top-level keys, and a block
# nested under one must not keep the previous top key. The knob is read
# wherever go-yaml merges one: the block's own indent (its first indented
# key fixes it — a deeper key is a nested block), keys bare or quoted, a
# space allowed before the colon, and the scoping rules of the parser:
# the last declaration inside the block wins, a column-0 re-open of the
# block in the same document drops what the earlier one held (go-yaml
# replaces the whole map), and a `---` document boundary keeps it (helm
# merges documents per key). It is never the runner's — values.yaml
# keeps the two side by side at the same indent.
rc=0
raw=$(awk '
  # A flow collection open past its opening line makes the file
  # unreadable line by line: go-yaml ignores indentation inside flow, so
  # no line reader can tell which key owns a later line. flow_scan
  # carries the brace depth across lines and returns nonzero for any
  # line after which a collection is still open. Braces inside a quoted
  # scalar or a comment do not count, and the braces of a plain value
  # are its own — in block context go-yaml reads `a:{b` and `https://…/{id}` as
  # one scalar — so a `{` opens a flow only at a value start, right
  # after the `key: ` separator.
  function flow_scan(s,    n, i, c, nxt, vs, ins, ids, esc, prev, q) {
    n = length(s)
    q = sprintf("%c", 39)
    vs = 1; ins = 0; ids = 0; esc = 0; prev = " "
    for (i = 1; i <= n; i++) {
      c = substr(s, i, 1)
      if (ids) {
        if (esc) esc = 0
        else if (c == "\\") esc = 1
        else if (c == "\"") ids = 0
      } else if (ins) {
        if (c == q) {
          if (substr(s, i + 1, 1) == q) i++
          else ins = 0
        }
      } else if (c == "#" && (prev == " " || prev == "\t")) {
        return depth != 0
      } else if (c == "\"") {
        ids = 1
      } else if (c == q) {
        ins = 1
      } else if (c == "{" || c == "[") {
        if (vs || depth > 0) depth++
        vs = 0
      } else if (c == "}" || c == "]") {
        if (depth > 0) depth--
        vs = 0
      } else if (c == ":") {
        nxt = substr(s, i + 1, 1)
        vs = nxt == "" || nxt == " " || nxt == "\t"
      } else if (c != " " && c != "\t") {
        vs = 0
      }
      prev = c
    }
    return depth != 0
  }
  # A literal (|) or folded (>) block scalar opens at a value start:
  # after the `key: ` separator the line carries only the indicator
  # with its optional indentation and chomping markers, and maybe a
  # trailing comment. bind records the indent of the opener so the
  # content lines can be told from the structure that follows the scalar.
  # unquote strips one pair of matching enclosing quotes — the quoted
  # scalar forms go-yaml reads — and leaves every other quote where it
  # is: a quote inside a plain scalar is a character of it, so `6"00` is
  # the string it spells, not 600.
  function unquote(s,    q, n) {
    q = sprintf("%c", 39)
    n = length(s)
    if (n >= 2 && ((substr(s, 1, 1) == "\"" && substr(s, n, 1) == "\"") || (substr(s, 1, 1) == q && substr(s, n, 1) == q)))
      return substr(s, 2, n - 2)
    return s
  }
  function block_open(s,    t) {
    t = s
    sub(/[ \t]+#.*/, "", t)
    sub(/[ \t]+$/, "", t)
    if (t !~ /^[ \t]*[^ \t#][^:]*:[ \t]*[|>][|+0-9-]*$/) return 0
    match(t, /^[ \t]*/)
    bind = RLENGTH
    return 1
  }
  BEGIN { doc = 0; sdoc = -1; in_block = 0; bind = 0 }
  {
    # A CRLF ending is the LF ending to go-yaml: the CR goes once, here,
    # so every rule below reads the line helm reads — `---` followed by
    # a CR is still a document boundary, `server:` still the block.
    sub(/\r$/, "")
    # Block scalar content is raw text: go-yaml reads no key in it, so
    # blank and deeper lines are skipped whole while a scalar is open;
    # any other line ends the scalar and is read like the rest.
    if (in_block && ($0 ~ /^[ \t]*$/ || (match($0, /^[ \t]+/) && RLENGTH > bind)))
      next
    in_block = 0
    if (flow_scan($0)) {
      bad = 1
      if (first == 0) first = NR
    }
    if (block_open($0))
      in_block = 1
  }
  # A column-0 --- starts a document: helm merges documents per key, so
  # the knob survives a boundary; doc counts the boundaries so a block
  # re-open is dropped only inside the document it repeats in.
  /^---([ \t]|$)/ { doc++ }
  # A column-0 key closes the previous block for the reader. A column-0
  # server: line re-opening the block in the SAME document drops the
  # remembered knob: go-yaml replaces the whole map on the repeat (helm
  # 4.1.1 renders no field for a knob the earlier block alone held), so
  # sizing the watch from it repeats the #2780 failure with the guard
  # green.
  /^[^ \t#]/ { top = $1; sub(/:$/, "", top); top = unquote(top); ind = 0; if (top == "server") { if (sdoc == doc) v = ""; sdoc = doc } }
  top == "server" && ind == 0 && $0 ~ /^[ \t]+[^ \t#]/ { match($0, /^[ \t]+/); ind = RLENGTH }
  top == "server" && ind > 0 && $0 !~ /^[ \t]*#/ && match($0, /^[ \t]+/) && RLENGTH == ind {
    line = $0
    key = line
    sub(/^[ \t]+/, "", key)
    sub(/:.*/, "", key)
    sub(/[ \t]+$/, "", key)
    key = unquote(key)
    if (key == "progressDeadlineSeconds") {
      v = line
      sub(/^[ \t]+/, "", v)
      sub(/^[^:]*:[ \t]*/, "", v)
      sub(/[ \t]+#.*/, "", v)
      sub(/[ \t]+$/, "", v)
      v = unquote(v)
      # A value resolving to one starting with `#` is null to go-yaml
      # (`key: # note`) or reads back empty in the applied manifest (a
      # quoted "#x" renders as `progressDeadlineSeconds: #x`, where the
      # whitespace before the # starts a comment): no deadline renders,
      # so it routes to the unset refusal, not the string one.
      if (substr(v, 1, 1) == "#") v = ""
    }
  }
  END { if (bad) { print first; exit 5 } print v }
' "$values") || rc=$?
case $rc in
  0) ;;
  5)
    echo "chart-kind-wait: $values opens a flow mapping or sequence that stays open past its opening line (first at line $raw) — go-yaml ignores indentation inside flow, so a line reader cannot tell which key owns a later line; write the knob plainly in the values file the install reads" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: could not read $values (awk exited $rc)" >&2
    exit 1
    ;;
esac

if [ -z "$raw" ]; then
  echo "chart-kind-wait: $values does not set server.progressDeadlineSeconds on a plain \`progressDeadlineSeconds:\` line nested under \`server:\` — that is the only form this guard reads; anchors and merge keys, flow mappings, and a scalar on the line after the key stay invisible to it — and the install would silently watch Kubernetes' default 600 s; write the knob plainly in the values file the install reads" >&2
  exit 1
fi

# Resolve the raw scalar the way go-yaml — the parser helm reads values
# with — hands it to the templates. Branch order mirrors go-yaml's own
# resolution: radix prefixes first, then a decimal that must not start
# with 0 (go-yaml reads those as octal or, on a digit outside [0-7],
# falls through to the float form), then the float form go-yaml resolves
# when the integer reads fail — 08 is 8. A value outside the exact awk
# integer range is refused: the watch is printed through awk doubles,
# which would round it and print a wait the file never set.
rc=0
deadline=$(printf '%s\n' "$raw" | awk '{
  s = $0
  neg = 0
  if (s ~ /^[+-]/) { neg = (substr(s, 1, 1) == "-"); s = substr(s, 2) }
  gsub(/_/, "", s)
  v = 0
  if (s ~ /^0[xX][0-9a-fA-F]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 16 + index("0123456789ABCDEF", toupper(substr(s, i, 1))) - 1
  } else if (s ~ /^0[bB][0-1]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 2 + (substr(s, i, 1) == "1")
  } else if (s ~ /^0[oO][0-7]+$/) {
    for (i = 3; i <= length(s); i++)
      v = v * 8 + (substr(s, i, 1) + 0)
  } else if (s ~ /^0[0-7]+$/) {
    for (i = 2; i <= length(s); i++)
      v = v * 8 + (substr(s, i, 1) + 0)
  } else if (s ~ /^(0|[1-9][0-9]*)$/) {
    v = s + 0
  } else if (s ~ /^(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][+-]?[0-9]+)?$/) {
    v = s + 0
    if (v != int(v)) exit 3
  } else {
    exit 3
  }
  if (v >= 9007199254740992) exit 4
  if (v == 0) neg = 0
  print (neg ? -v : v)
}') || rc=$?
case $rc in
  0) ;;
  3)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is not an integer — go-yaml hands it to the templates verbatim and the API refuses the Deployment" >&2
    exit 1
    ;;
  4)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is beyond the exact integer range this guard resolves — the printed wait would not be the deadline the file sets" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: could not resolve server.progressDeadlineSeconds = '$raw' in $values (awk exited $rc)" >&2
    exit 1
    ;;
esac
case $deadline in
  -*)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is negative — a negative deadline cannot budget the install watch; set a real deadline" >&2
    exit 1
    ;;
esac
if [ "$deadline" -eq 0 ]; then
  echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values renders through a \`with\` that skips it — Kubernetes' 600 s default applies; set a real deadline" >&2
  exit 1
fi

# The deadline budgets the server Deployment alone; the wait also covers the
# stores and the release plumbing, and helm needs a breath after the moment
# the deadline can fire.
margin=120
awk -v d="$deadline" -v m="$margin" 'BEGIN { printf "%ds\n", d + m }'
