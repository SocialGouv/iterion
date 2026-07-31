#!/usr/bin/env python3
"""Combien des mutations que le FILET détecte la SUITE DE TESTS détecte-t-elle ?

Les deux instruments voient le même dépôt et prétendent tous deux protéger d'une
régression. Ils ne protègent pas de la même chose, et la seule façon honnête de
le dire est de les soumettre au même jeu d'épreuves : chaque mutant du
contre-test est appliqué, la suite héritée est lancée, et l'on note si elle
rougit.

CE QUE CE PROGRAMME NE DIT PAS. Qu'un test unitaire ne voie pas une mutation
n'en fait pas un mauvais test : une suite unitaire a d'autres vertus — elle
localise la faute, elle tourne en secondes, elle documente une intention. Le
chiffre ci-dessous mesure UNE chose : la couverture comportementale de ce que
l'application sert réellement. Le lire comme une note de qualité serait
exactement l'abus que ce dépôt reproche ailleurs.

Usage : devbox run -- python3 .golden-master/suite-vs-net.py [--jobs-report FICHIER]
"""
import json
import os
import re
import subprocess
import sys
import time

GM = os.environ.get("GM_DIR", ".golden-master")
TIMEOUT = 1800

# La commande de test et l'emplacement des rapports viennent de `config.json` :
# ce programme ne connaît aucun outil de build. Deux clés, `test_cmd` et
# `test_results_dir`, et il refuse de tourner sans elles plutôt que de deviner.
#
# La commande DOIT forcer la ré-exécution. Un outil de build qui considère sa
# tâche de test à jour quand ses entrées n'ont pas bougé la saute — et une
# mutation de DONNÉES n'en touche aucune. La première version de ce programme
# mesurait alors « la suite n'a pas vu la mutation » là où la suite n'avait pas
# tourné du tout : trois secondes pour 326 cas, ce qui aurait dû sauter aux yeux.
# Le compte de cas réellement exécutés, lu plus bas, est le garde-fou qui rend
# cette erreur impossible à répéter en silence.


def run(cmd, timeout=TIMEOUT):
    p = subprocess.run(["sh", "-c", cmd], stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True, timeout=timeout)
    return p.returncode, p.stdout or ""


def tests_executed(results_dir):
    """Combien de cas la dernière exécution a réellement lancés.

    Le compte est lu dans les rapports produits, pas déduit du code de sortie.
    Un `test` sauté sort 0 exactement comme un `test` vert, et c'est cette
    confusion qui rendait la première mesure vide.
    """
    total = 0
    for name in os.listdir(results_dir) if os.path.isdir(results_dir) else []:
        if not name.endswith(".xml"):
            continue
        with open(os.path.join(results_dir, name), encoding="utf-8", errors="replace") as f:
            head = f.read(600)
        m = re.search(r'tests="(\d+)"', head)
        if m:
            total += int(m.group(1))
    return total


def mutants():
    root = os.path.join(GM, "mutants")
    out = []
    for name in sorted(os.listdir(root)):
        d = os.path.join(root, name)
        meta = os.path.join(d, "meta.json")
        if name in ("holdout", "audit") or not os.path.isdir(d) or not os.path.isfile(meta):
            continue
        with open(meta, encoding="utf-8") as f:
            m = json.load(f)
        m["id"], m["dir"] = name, d
        out.append(m)
    return out


def main():
    cfg_path = os.path.join(GM, "config.json")
    with open(cfg_path, encoding="utf-8") as f:
        cfg = json.load(f)
    test_cmd = cfg.get("test_cmd")
    results_dir = cfg.get("test_results_dir")
    if not test_cmd or not results_dir:
        print("config.json doit déclarer `test_cmd` et `test_results_dir`. Ce programme "
              "ne connaît aucun outil de build, et deviner en ferait un qui mesure "
              "parfois autre chose que ce qu'il annonce.")
        return 2

    # Point de départ VERT exigé. Une suite déjà rouge rendrait « détectée »
    # pour chaque mutant, et le chiffre serait parfait et vide.
    print("== état de départ : la suite doit être verte avant de muter quoi que ce soit")
    code, out = run(test_cmd)
    if code != 0:
        print("la suite est ROUGE sur HEAD — rien à mesurer tant qu'elle ne passe pas :")
        print(out[-1500:])
        return 2
    baseline_count = tests_executed(results_dir)
    if baseline_count == 0:
        print("aucun cas de test n'a été exécuté sur HEAD — la mesure serait vide")
        return 2
    print("   verte, %d cas exécutés\n" % baseline_count)

    rows = []
    for m in mutants():
        t0 = time.time()
        code, _ = run("sh %s/apply.sh" % m["dir"])
        if code != 0:
            rows.append((m["id"], m["surface"], m["archetype"], "MUTANT INVALIDE"))
            run("sh %s/revert.sh" % m["dir"])
            continue
        try:
            code, out = run(test_cmd)
            seen = code != 0
            ran = tests_executed(results_dir)
        finally:
            run("sh %s/revert.sh" % m["dir"])
        # « La suite n'a pas vu » n'a de sens que si la suite a TOURNÉ. Un
        # compte de cas plus faible que celui de départ veut dire qu'une partie
        # a été sautée, et le verdict de ce mutant ne vaut rien.
        if not seen and ran < baseline_count:
            verdict = "MESURE VIDE (%d cas)" % ran
        else:
            verdict = "VUE" if seen else "non vue"
        rows.append((m["id"], m.get("surface", "http"), m.get("archetype", "?"), verdict))
        print("   %-36s %-7s %-15s %-22s %4d cas (%.0f s)"
              % (m["id"], m.get("surface"), m.get("archetype"), verdict, ran, time.time() - t0))

    seen = sum(1 for r in rows if r[3] == "VUE")
    empty = [r[0] for r in rows if r[3].startswith("MESURE VIDE")]
    total = len(rows)
    if empty:
        print("\n   MESURES VIDES, à ne pas compter : %s" % ", ".join(empty))
    print("\n== RESULTAT")
    print("   le filet détecte      : %d / %d" % (total, total))
    print("   la suite héritée      : %d / %d" % (seen, total))
    print("\n   par surface :")
    for surf in sorted({r[1] for r in rows}):
        sub = [r for r in rows if r[1] == surf]
        print("     %-8s %d / %d" % (surf, sum(1 for r in sub if r[3] == "VUE"), len(sub)))
    with open(os.path.join(GM, "suite-vs-net.json"), "w", encoding="utf-8") as f:
        json.dump({"net_detected": total, "suite_detected": seen,
                   "rows": [{"mutant": a, "surface": b, "archetype": c, "suite": d}
                            for a, b, c, d in rows]}, f, indent=2, ensure_ascii=False)
    print("\n   écrit dans %s/suite-vs-net.json" % GM)
    return 0


if __name__ == "__main__":
    sys.exit(main())
