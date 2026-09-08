#!/usr/bin/env python3
"""Regenerate the inlined copy of the floor from the standalone one.

The floor exists twice: inlined in `main.bot`'s `inventory_floor` node — the
copy that actually RUNS — and as `floor.py`, the copy a human reviews. Keeping
them in sync by hand is an obligation nobody verifies, so it drifts; this script
does the mechanical part and `TestScopeFloorCopiesStayInSync` holds the result
byte-identical.

Run from the repository root: `python3 bots/modernize-scope/sync-floor.py`.
"""

import io
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
STANDALONE = os.path.join(HERE, "floor.py")
BOT = os.path.join(HERE, "main.bot")

BEGIN = "# ---- shared body below (kept byte-identical with the inlined copy) ----"
END = "# ---- shared body above ----"
PREAMBLE_END = "    # ---- inlined floor below"
EPILOGUE_START = "    # ---- inlined floor above"


def shared_body(text):
    """Everything between the markers, markers excluded. The CLI wrapper stays
    out: the inlined copy is driven by the node's preamble instead."""
    if BEGIN not in text or END not in text:
        raise SystemExit("floor.py: the shared-body markers are missing — fix this script, not the markers")
    return text.split(BEGIN, 1)[1].split(END, 1)[0].strip("\n")


def indented(body):
    return "\n".join(("    " + l) if l.strip() else "" for l in body.split("\n"))


def main():
    body = shared_body(io.open(STANDALONE, encoding="utf-8").read())
    bot = io.open(BOT, encoding="utf-8").read()
    if PREAMBLE_END not in bot or EPILOGUE_START not in bot:
        raise SystemExit("main.bot: the inline markers are missing")
    head = bot.split(PREAMBLE_END, 1)[0] + PREAMBLE_END + "\n"
    tail = EPILOGUE_START + bot.split(EPILOGUE_START, 1)[1]
    io.open(BOT, "w", encoding="utf-8").write(head + indented(body) + "\n" + tail)
    print("inlined copy regenerated from floor.py (%d body lines)" % (body.count("\n") + 1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
