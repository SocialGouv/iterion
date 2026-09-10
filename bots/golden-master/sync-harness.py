#!/usr/bin/env python3
"""Re-inline `oracle-harness.py` into its inlined copies: the `oracle_run` node
of `main.bot` and the `sync_harness` node of `sync-harness.bot`.

The two copies of the harness are kept BYTE-IDENTICAL by
`bots/golden_master_harness_sync_test.go`: the one that runs is inlined in the
`.bot`, the one that is read is this standalone file. Editing one and
regenerating the other is the gesture; this script is that gesture, so it is
never done by hand.

What CANNOT be shared, and what this script preserves: the node's preamble,
which binds the graph's `{{vars.*}}` into the environment and only makes sense
there; and the standalone header (shebang, module docstring), which only makes
sense outside the block.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
STANDALONE = os.path.join(HERE, "oracle-harness.py")
BODY_START = "import hashlib"
# Each inlined copy: (file, last line of the preamble). The body is re-inlined
# right after that line, up to the next non-indented line.
TARGETS = [
    (os.path.join(HERE, "main.bot"), "os.environ['GM_MODE'] = 'gate'"),
    (os.path.join(HERE, "sync-harness.bot"), "# ---- inlined harness below"),
]


def reinject(bot, preamble_end, body):
    name = os.path.basename(bot)
    lines = open(bot, encoding="utf-8").read().split("\n")

    # The line that IS the end of the preamble (an assignment that quotes the
    # marker inside a string is not one).
    starts = [i for i, l in enumerate(lines)
              if preamble_end in l and not l.strip().startswith(("MARK", '"', "'"))]
    if len(starts) != 1:
        sys.exit("%s: %d preamble end(s) %r — expected exactly one"
                 % (name, len(starts), preamble_end))
    start = starts[0] + 1

    end = next((i for i in range(start, len(lines))
                if lines[i].strip() and not lines[i].startswith("    ")), None)
    if end is None:
        sys.exit("%s: the harness block has no end — is the node the last of the file?" % name)

    lines[start:end] = ["    " + l if l.strip() else "" for l in body] + [""]
    open(bot, "w", encoding="utf-8").write("\n".join(lines))
    print("%s: %d harness lines re-inlined from oracle-harness.py" % (name, len(body)))


def main():
    sl = open(STANDALONE, encoding="utf-8").read().split("\n")
    body_at = [j for j, l in enumerate(sl) if l == BODY_START]
    if not body_at:
        sys.exit("oracle-harness.py: first body line %r not found — were the imports reordered?"
                 % BODY_START)
    body = sl[body_at[0]:]
    while body and not body[-1].strip():
        body.pop()
    for bot, preamble_end in TARGETS:
        reinject(bot, preamble_end, body)


main()
