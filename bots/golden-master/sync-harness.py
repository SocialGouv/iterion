#!/usr/bin/env python3
"""Reinjecte `oracle-harness.py` dans le noeud `oracle_run` de `main.bot`.

Les deux copies du harnais sont tenues BYTE-IDENTIQUES par
`bots/golden_master_harness_sync_test.go` : celle qui tourne est inlinee dans le
`.bot`, celle qu'on relit est ce fichier standalone. Editer l'une et regenerer
l'autre est le geste ; ce script est ce geste, pour qu'il ne se fasse pas a la
main.

Ce qui NE peut pas etre partage, et que ce script preserve : le preambule du
noeud, qui lie les `{{vars.*}}` du graphe dans l'environnement et n'a de sens
que la ; et l'entete standalone (shebang, docstring de module) qui n'a de sens
que hors du bloc.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BOT = os.path.join(HERE, "main.bot")
STANDALONE = os.path.join(HERE, "oracle-harness.py")
PREAMBLE_END = "os.environ['GM_MODE'] = 'gate'"
BODY_START = "import hashlib"


def main():
    lines = open(BOT, encoding="utf-8").read().split("\n")

    starts = [i for i, l in enumerate(lines) if PREAMBLE_END in l]
    if len(starts) != 1:
        sys.exit("main.bot : %d fin(s) de preambule %r — attendu exactement une"
                 % (len(starts), PREAMBLE_END))
    start = starts[0] + 1

    end = next((i for i in range(start, len(lines))
                if lines[i].strip() and not lines[i].startswith("    ")), None)
    if end is None:
        sys.exit("main.bot : le bloc du harnais n'a pas de fin — le noeud est le dernier du fichier ?")

    sl = open(STANDALONE, encoding="utf-8").read().split("\n")
    body_at = [j for j, l in enumerate(sl) if l == BODY_START]
    if not body_at:
        sys.exit("oracle-harness.py : premiere ligne de corps %r introuvable — imports reordonnes ?"
                 % BODY_START)
    body = sl[body_at[0]:]
    while body and not body[-1].strip():
        body.pop()

    lines[start:end] = ["    " + l if l.strip() else "" for l in body] + [""]
    open(BOT, "w", encoding="utf-8").write("\n".join(lines))
    print("main.bot : %d lignes de harnais reinjectees depuis oracle-harness.py" % len(body))


main()
