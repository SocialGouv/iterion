#!/usr/bin/env python3
"""Reinjecte `oracle-harness.py` dans ses copies inlinees : le noeud `oracle_run`
de `main.bot` et le noeud `sync_harness` de `sync-harness.bot`.

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
STANDALONE = os.path.join(HERE, "oracle-harness.py")
BODY_START = "import hashlib"
# Chaque copie inlinee : (fichier, derniere ligne du preambule). Le corps est
# reinjecte juste apres cette ligne, jusqu'a la prochaine ligne non indentee.
TARGETS = [
    (os.path.join(HERE, "main.bot"), "os.environ['GM_MODE'] = 'gate'"),
    (os.path.join(HERE, "sync-harness.bot"), "# ---- inlined harness below"),
]


def reinject(bot, preamble_end, body):
    name = os.path.basename(bot)
    lines = open(bot, encoding="utf-8").read().split("\n")

    # La ligne qui EST la fin du preambule (une affectation qui cite le
    # marqueur dans une chaine n'en est pas une).
    starts = [i for i, l in enumerate(lines)
              if preamble_end in l and not l.strip().startswith(("MARK", '"', "'"))]
    if len(starts) != 1:
        sys.exit("%s : %d fin(s) de preambule %r — attendu exactement une"
                 % (name, len(starts), preamble_end))
    start = starts[0] + 1

    end = next((i for i in range(start, len(lines))
                if lines[i].strip() and not lines[i].startswith("    ")), None)
    if end is None:
        sys.exit("%s : le bloc du harnais n'a pas de fin — le noeud est le dernier du fichier ?" % name)

    lines[start:end] = ["    " + l if l.strip() else "" for l in body] + [""]
    open(bot, "w", encoding="utf-8").write("\n".join(lines))
    print("%s : %d lignes de harnais reinjectees depuis oracle-harness.py" % (name, len(body)))


def main():
    sl = open(STANDALONE, encoding="utf-8").read().split("\n")
    body_at = [j for j, l in enumerate(sl) if l == BODY_START]
    if not body_at:
        sys.exit("oracle-harness.py : premiere ligne de corps %r introuvable — imports reordonnes ?"
                 % BODY_START)
    body = sl[body_at[0]:]
    while body and not body[-1].strip():
        body.pop()
    for bot, preamble_end in TARGETS:
        reinject(bot, preamble_end, body)


main()
