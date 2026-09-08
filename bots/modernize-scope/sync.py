#!/usr/bin/env python3
"""Regenerate every inlined copy of the deterministic core from the standalone one.

The core exists once as `scope.py` — the copy a human reviews — and once per tool
node in `main.bot` that runs it. Keeping them in sync by hand is an obligation
nobody verifies, so it drifts; this script does the mechanical part and
`TestScopeCopiesStayInSync` holds the result byte-identical.

Every `# ---- inlined body below` / `# ---- inlined body above` pair in the bot is
a site, and all of them get the same body. Run from the repository root:
`python3 bots/modernize-scope/sync.py`.
"""

import io
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
STANDALONE = os.path.join(HERE, "scope.py")
BOT = os.path.join(HERE, "main.bot")

BEGIN = "# ---- shared body below (kept byte-identical with the inlined copy) ----"
END = "# ---- shared body above ----"
BELOW = "    # ---- inlined body below"
ABOVE = "    # ---- inlined body above"


def shared_body(text):
    """Everything between the markers, markers excluded. The CLI wrapper stays
    out: the inlined copies are driven by each node's preamble instead."""
    if BEGIN not in text or END not in text:
        raise SystemExit("scope.py: the shared-body markers are missing — fix this script, not the markers")
    return text.split(BEGIN, 1)[1].split(END, 1)[0].strip("\n")


def indented(body):
    return "\n".join(("    " + l) if l.strip() else "" for l in body.split("\n"))


def main():
    body = shared_body(io.open(STANDALONE, encoding="utf-8").read())
    bot = io.open(BOT, encoding="utf-8").read()
    if bot.count(BELOW) == 0 or bot.count(BELOW) != bot.count(ABOVE):
        raise SystemExit("main.bot: %d opening and %d closing inline markers — the markers are the "
                         "contract, restore them" % (bot.count(BELOW), bot.count(ABOVE)))
    out, rest, sites = "", bot, 0
    while BELOW in rest:
        head, rest = rest.split(BELOW, 1)
        _, rest = rest.split(ABOVE, 1)
        out += head + BELOW + "\n" + indented(body) + "\n" + ABOVE
        sites += 1
    io.open(BOT, "w", encoding="utf-8").write(out + rest)
    print("%d inlined copies regenerated from scope.py (%d body lines each)"
          % (sites, body.count("\n") + 1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
