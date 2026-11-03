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
# with, scoped the way go-yaml — the parser helm reads values with —
# scopes it: inside a document a repeated `server:` block replaces the
# whole map (helm 3.20.2 and 4.1.1 alike), so a knob declared only in the
# earlier block renders no field and Kubernetes' 600 s default applies,
# and a knob the later block declares is the one that renders. The reader
# drops the knob — and a continuation it recorded under it — when a
# column-0 `server:` line re-opens the block. A second document is where
# the two helm majors the install sites may run part ways: helm 3.20.2
# (the devbox pin, probe-measured) reads the first document alone, helm
# 4.1.1 merges `---` documents per key, and CI's setup-helm pins no
# version — so a values file whose content continues past a document
# boundary (a column-0 `---` or `...`) is refused instead of sized for
# one helm and wrong for the other. A column-0 `---` before any content
# is the explicit start of the first document, and a boundary that only
# blank or comment lines follow opens a document neither helm reads
# anything from, so neither is refused.
#
# The extractor reads the plain block form: the knob nested under
# `server:` at whatever uniform indent the block's keys use — the block's
# first indented key fixes that indent, and a deeper key belongs to a
# nested block, not to the knob — with the top key and the knob key bare
# or quoted, the key's colon followed by a space, a tab or the line end
# (the only separator go-yaml reads in block context: it reads
# `progressDeadlineSeconds:600` as one plain scalar, and helm refuses the
# file), and the value on the same line. Indentation is spaces: go-yaml
# reads no tab in the indentation of any line — a key, a comment, a
# whitespace-only line, or the content of a block scalar short of its
# indent column — and helm refuses such a file, so the guard refuses it
# at its first tab-indented line rather than sizing a watch for an
# install that never starts. What a line reader cannot resolve is refused
# loudly, never guessed: anchors and merge keys, a flow mapping or
# sequence that stays open past its opening line, and a scalar on the
# line after the key stay invisible to it, and a value continued onto a
# deeper line is one string to go-yaml (`600` then `0` is "600 0") where
# a line reader sees its first line alone; such a file is refused rather
# than sized from a value helm does not use. go-yaml ignores indentation
# inside flow, so a line reader cannot tell which key owns a later line —
# a flow interior read as a column-0 key re-anchors the reader and a knob
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
# flow. (The merge-key route cannot reach an install either way: the
# chart's values schema rejects the top-level anchor holder it needs.)
#
# The knob's value is resolved the way go-yaml — the parser helm reads
# values with — resolves it, so a file helm accepts is not refused here as
# "not an integer": an optional sign and `_` separators are cosmetic,
# 0x/0b/0o and a leading-0 form carry their radix (0600 is octal 384, not
# 600), and a float that is integral lands as its integer (600.0 renders
# as 600). A quoted decimal is the one string form that installs anyway:
# the templates print the scalar verbatim and the applied manifest reads
# it back as the integer it spells (helm 4.1.1 renders "600"
# byte-identical to 600), so it is accepted here. Only one pair of
# enclosing quotes is that form: a quote inside a plain scalar is a
# character of it (`6"00` is the string it spells), and a single-quoted
# `'"600"'` keeps its inner quotes through the render, so both are
# refused as the strings go-yaml hands over. Every other value is refused
# as one this guard does not resolve, and the refusal names the guard's
# reach, not the API's verdict: some such values helm renders verbatim
# and the API refuses (a word, base-60 like 1:30, a non-integral float),
# while others helm resolves to a deadline the install runs with (`&d
# 600`, `!!int 600`, a literal block holding 600, `"600 # x"`, `" 600"` —
# helm 3.20.2 renders each) — the guard proves neither, so it refuses
# both and asks for the plain integer it reads. A negative deadline is
# refused as well: it cannot budget the watch. So is a value beyond the
# int32 the Deployment field holds (2147483647 at most): no install can
# carry it, and below it the margin stays exact in the doubles awk
# computes with.
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
# unset refusal instead of claiming an API refusal helm never sees. So
# do a null spelling (`~`, `null`, `Null`, `NULL` — bare, the `with`
# skips it; quoted, it renders as a null the applied manifest reads back
# as empty) and a bare false (`false`, `no`, `off` in any case: booleans
# to helm's values parser, which the `with` skips as well).
# Usage: scripts/chart-kind-wait.sh <values file>
set -eu

values=${1:?usage: chart-kind-wait.sh <values file>}
[ -f "$values" ] || { echo "chart-kind-wait: no values file at $values" >&2; exit 1; }

# Any column-0 key closes the previous block — whatever it starts with:
# go-yaml accepts `_helpers:` or `2nd:` as top-level keys, and a block
# nested under one must not keep the previous top key. The knob is read
# wherever go-yaml merges one: the block's own indent (its first indented
# key fixes it — a deeper key is a nested block), keys bare or quoted, the
# key's colon followed by whitespace or the line end, and the scoping
# rule of the parser: the last declaration inside the block wins, and a
# column-0 re-open of the block drops what the earlier one held (go-yaml
# replaces the whole map). Content past a document boundary is refused
# before any of this applies. The knob is never the runner's —
# values.yaml keeps the two side by side at the same indent.
rc=0
raw=$(LC_ALL=C awk '
  # A flow collection open past its opening line makes the file
  # unreadable line by line: go-yaml ignores indentation inside flow, so
  # no line reader can tell which key owns a later line. flow_scan
  # carries the brace depth across lines and returns nonzero for any
  # line after which a collection is still open. Braces inside a quoted
  # scalar or a comment do not count, and the braces of a plain value
  # are its own — in block context go-yaml reads `a:{b` and `https://…/{id}` as
  # one scalar — so a `{` opens a flow only at a value start: right
  # after the `key: ` separator, after a `- ` entry or `? ` key
  # indicator, or after a `!tag` or `&anchor` property standing at one.
  function flow_scan(s,    n, i, c, nxt, ws, vs, ins, ids, esc, prev, q) {
    n = length(s)
    q = sprintf("%c", 39)
    vs = 1; ins = 0; ids = 0; esc = 0; prev = " "
    for (i = 1; i <= n; i++) {
      c = substr(s, i, 1)
      nxt = substr(s, i + 1, 1)
      ws = nxt == "" || nxt == " " || nxt == "\t"
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
        vs = ws
      } else if (vs && (c == "-" || c == "?") && ws) {
        # A `- ` entry or `? ` key indicator at a value start opens a
        # nested node, so the value start carries over to what follows:
        # `- {`, `- - [` and `? {` open a flow like a `{` after `key: `.
        # Glued to its scalar (`-1`, `?x`) the character belongs to the scalar.
        vs = 1
      } else if (vs && (c == "!" || c == "&")) {
        # A tag or an anchor at a value start is a property of the node
        # that follows it: the token runs to the next whitespace and the
        # value start survives it, so `!!map {` and `&x {` open a flow.
        # Past a value start, `!` and `&` are characters of a plain scalar.
        while (i < n && substr(s, i + 1, 1) != " " && substr(s, i + 1, 1) != "\t") i++
        c = substr(s, i, 1)
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
  # content lines can be told from the structure that follows the
  # scalar; bcol, the indent column the first content line fixes, is
  # reset for the scalar to come.
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
    bcol = 0
    return 1
  }
  BEGIN { doc = 0; bound = 0; started = 0; content = 0; multi = 0; in_block = 0; bind = 0; bcol = 0; tab = 0; pend = 0; pind = 0; cont = 0; seen = 0 }
  {
    # A CRLF ending is the LF ending to go-yaml: the CR goes once, here,
    # so every rule below reads the line helm reads — `---` followed by
    # a CR is still a document boundary, `server:` still the block.
    sub(/\r$/, "")
    # Block scalar content is raw text: go-yaml reads no key in it. The
    # first content line fixes the indent column of the scalar, and from
    # then on every line indented at least that far is content, whatever
    # it holds — a tab past the column is a character of the string. A
    # tab go-yaml meets short of the column — in the leading whitespace
    # of the first content line, or on a later line, blank or not — is no
    # indentation to it ("found a tab character where an indentation
    # space is expected") and helm refuses the file. A line no deeper
    # than the opener ends the scalar and is read like the rest.
    if (in_block) {
      match($0, /^ */)
      sp = RLENGTH
      if (bcol == 0) {
        if ($0 ~ /^ *$/) next
        if (sp > bind) {
          if (substr($0, sp + 1, 1) == "\t") { if (!tab) tab = NR }
          else bcol = sp
          next
        }
      } else {
        if (sp >= bcol) next
        if (substr($0, sp + 1, 1) == "\t") { if (!tab) tab = NR; next }
        if ($0 ~ /^ *$/ || sp > bind) next
      }
    }
    in_block = 0
    # A structure line indented with a tab — a key, a comment or a
    # whitespace-only line, at any depth — is refused by go-yaml ("found
    # character that cannot start any token"), so helm never installs
    # the file; the first such line is named.
    if (!tab && $0 ~ /^ *\t/) tab = NR
    # A plain scalar continues onto any deeper content line that follows
    # it — blank lines fold into it, a comment line is skipped — so the
    # knob is then the multi-line string go-yaml hands over (`600` then
    # `0` is "600 0", which helm renders verbatim and the API refuses),
    # or go-yaml refuses the file (a deeper `key: value`, or content
    # after a comment); the integer the first line spells is the value
    # in neither case. pend is raised by a knob line that carries a
    # value and lowered by the next content line, which is recorded as
    # the continuation when it is deeper than the knob.
    if (pend && $0 !~ /^[ \t]*(#.*)?$/) {
      if (match($0, /^[ \t]+/) && RLENGTH > pind && cont == 0) cont = NR
      pend = 0
    }
    if (flow_scan($0)) {
      bad = 1
      if (first == 0) first = NR
    }
    if (block_open($0))
      in_block = 1
    # A content line — neither blank, comment-only nor a document marker
    # — past a document boundary is the second document the two helm
    # majors read differently; the first such line is named.
    if ($0 !~ /^[ \t]*(#.*)?$/ && $0 !~ /^(---|\.\.\.)([ \t]|$)/) {
      if (doc > 0 && !multi) multi = NR
      content = 1
    }
  }
  # A column-0 --- before any content is the explicit start of the first
  # document, which helm reads. Every other column-0 --- or ... closes
  # the document helm 3.20.2 reads and opens one it ignores while helm
  # 4.1.1 merges it per key, so doc counts them and the content that
  # follows one is refused above.
  /^(---|\.\.\.)([ \t]|$)/ {
    if ($0 ~ /^---/ && !content && !started) started = 1
    else { doc++; if (!bound) bound = NR }
  }
  # A column-0 key closes the previous block for the reader. A column-0
  # server: line re-opening the block drops the remembered knob and the
  # continuation recorded under it: go-yaml replaces the whole map on the
  # repeat (helm 3.20.2 renders the later block alone: no field for a
  # knob the earlier block alone held, and 1200 for `600` continued by a
  # deeper `0` then re-opened with 1200), so sizing the watch from the
  # earlier knob repeats the #2780 failure with the guard green, and
  # refusing the earlier continuation refuses a file helm installs.
  /^[^ \t#]/ { top = $1; sub(/:$/, "", top); top = unquote(top); ind = 0; if (top == "server") { if (seen) { v = ""; cont = 0 }; seen = 1 } }
  top == "server" && ind == 0 && $0 ~ /^[ \t]+[^ \t#]/ { match($0, /^[ \t]+/); ind = RLENGTH }
  top == "server" && ind > 0 && $0 !~ /^[ \t]*#/ && match($0, /^[ \t]+/) && RLENGTH == ind {
    rest = $0
    sub(/^[ \t]+/, "", rest)
    # The key ends at the first colon a space, a tab or the line end
    # follows — the only separator go-yaml reads in block context. A
    # colon glued to what follows it is a character of the plain scalar
    # (`progressDeadlineSeconds:600` is one string to go-yaml, and helm
    # refuses the file: server is no object then, or "mapping values are
    # not allowed in this context"), so such a line is no knob line.
    if (match(rest, /:([ \t]|$)/)) {
      key = substr(rest, 1, RSTART - 1)
      sub(/[ \t]+$/, "", key)
      key = unquote(key)
      if (key == "progressDeadlineSeconds") {
        v = substr(rest, RSTART + 1)
        sub(/^[ \t]+/, "", v)
        sub(/[ \t]+#.*/, "", v)
        sub(/[ \t]+$/, "", v)
        quoted = (unquote(v) != v)
        v = unquote(v)
        # A value resolving to one starting with `#` is null to go-yaml
        # (`key: # note`) or reads back empty in the applied manifest (a
        # quoted "#x" renders as `progressDeadlineSeconds: #x`, where the
        # whitespace before the # starts a comment): no deadline renders,
        # so it routes to the unset refusal, not the string one.
        if (substr(v, 1, 1) == "#") v = ""
        # The same verdict for a null spelling: go-yaml resolves `~`,
        # `null`, `Null` and `NULL` to null, which the `with` of the templates
        # skips, and the quoted spelling renders as
        # `progressDeadlineSeconds: null`, which the applied manifest reads
        # back as empty (helm 3.20.2 renders both). A bare false — `false`,
        # `False`, `FALSE`, and the YAML 1.1 spellings n, no and off in any
        # case, booleans to the values parser of helm — is skipped by the `with`
        # as well; quoted, it is a string that renders as a boolean the API
        # refuses, so only the bare form routes to the unset refusal.
        if (v ~ /^(~|null|Null|NULL)$/) v = ""
        if (!quoted && v ~ /^(false|False|FALSE|n|N|no|No|NO|off|Off|OFF)$/) v = ""
        # A value on the knob line may continue below it: the next content
        # line decides (the continuation check above), so a knob read as
        # unset or null raises nothing.
        pend = (v != "")
        pind = ind
      }
    }
  }
  END {
    if (bad) { print first; exit 5 }
    if (tab) { print tab; exit 8 }
    if (multi) { print bound, multi; exit 7 }
    if (cont) { print cont; exit 6 }
    print v
  }
' "$values") || rc=$?
case $rc in
  0) ;;
  5)
    echo "chart-kind-wait: $values opens a flow mapping or sequence that stays open past its opening line (first at line $raw) — go-yaml ignores indentation inside flow, so a line reader cannot tell which key owns a later line; write the knob plainly in the values file the install reads" >&2
    exit 1
    ;;
  6)
    echo "chart-kind-wait: $values continues the server.progressDeadlineSeconds value onto line $raw — go-yaml folds a deeper line into the plain scalar above it (\`600\` then \`0\` is the string \"600 0\", which helm renders verbatim and the API refuses the Deployment) or refuses the file outright, so the knob is not the integer its first line spells; write the value on the knob's own line" >&2
    exit 1
    ;;
  7)
    set -- $raw
    echo "chart-kind-wait: $values holds more than one document — content at line $2 follows the document boundary at line $1 — and the helm majors the install sites may run read it differently (helm 3 reads the first document alone, helm 4 merges documents per key), so no watch fits both; write the values in one document" >&2
    exit 1
    ;;
  8)
    echo "chart-kind-wait: $values indents line $raw with a tab — go-yaml reads no tab as indentation, so helm refuses the file and no watch can be sized for it; indent with spaces" >&2
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
# when the integer reads fail — 08 is 8. The deadline must fit the int32
# the Deployment field holds: no install carries a larger one, and the
# margin added to any int32 stays exact in the doubles awk computes with.
# Every awk here runs under LC_ALL=C: mawk reads numbers through the
# host locale, where a comma-decimal LC_NUMERIC stops "600.5" at the dot
# and resolves it to 600 — an integer the file never set.
rc=0
deadline=$(printf '%s\n' "$raw" | LC_ALL=C awk '{
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
  if (v == 0) neg = 0
  signed = (neg ? -v : v)
  if (signed > 2147483647 || signed < -2147483648) exit 4
  print signed
}') || rc=$?
case $rc in
  0) ;;
  3)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is not an integer this guard resolves — it reads a bare or quoted decimal and the integer spellings go-yaml resolves (a sign, _ separators, 0x/0o/0b, a leading-0 octal, an integral float); a tag, an anchor, a block scalar or whitespace inside the quotes may still render a deadline for helm, or the value may render verbatim for the API to refuse, and the guard proves neither; write the deadline as a plain integer on the knob's line" >&2
    exit 1
    ;;
  4)
    echo "chart-kind-wait: server.progressDeadlineSeconds = '$raw' in $values is beyond the int32 the Deployment field holds (2147483647 at most) — no install can carry it, so the printed wait would not be a deadline the install runs with" >&2
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
# the deadline can fire. The sum is integral, so %.0f prints it exactly on
# every awk — busybox's %d clamps a value past int32 to -2147483648.
margin=120
LC_ALL=C awk -v d="$deadline" -v m="$margin" 'BEGIN { printf "%.0fs\n", d + m }'
