# vibe_review_alternating — companion notes

Compagnon du workflow [`vibe_review_alternating.iter`](vibe_review_alternating.iter). Ce document est un **journal de design** : ce qui marche en pratique, ce qui reste un problème ouvert, et ce qu'il faudrait améliorer.

## Intention du workflow

Faire alterner deux reviewers de familles distinctes (Claude Opus via `claude_code`, GPT-5.5 via `claw`) sur **toute la base de code** pour atteindre un état production-ready, avec :

- Un fixer par famille qui hérite de la session du reviewer correspondant (économie de cache + continuité de contexte)
- Une condition d'arrêt déterministe : deux verdicts positifs consécutifs de familles opposées (cross-family double approval)
- Un budget par boucle (`review_loop(15)` aujourd'hui, ajustable selon la complexité du projet)

## Pattern de convergence observé en pratique

Méthode appliquée manuellement avec Claude Code sur des projets de complexité comparable (engine + SDK vendored + DSL) : **5 à 40+ itérations** avant convergence selon la maturité du code et la sévérité du curseur. Le profil typique :

- **Itérations précoces** : densité élevée de blockers réels (bugs concrets, races, fuites, vulnérabilités). Le fixer applique, le code progresse.
- **Itérations intermédiaires** : densité décroissante. Les reviewers commencent à proposer des améliorations plus subtiles. La discipline "blocker = casse la prod" devient critique pour ne pas glisser vers le perfectionnisme.
- **Itérations tardives** : redondance avec les passes précédentes, faux-positifs auto-détectés via `previous_scanned_areas`, blockers de nature stylistique ou hypothétique. **C'est le signal de convergence asymptotique.**
- **Signal d'alarme** : explosion soudaine de blockers critiques tard dans le run. Hypothèses : reviewer qui hallucine, fixer qui n'applique pas réellement, ou re-flagging d'items déjà pushbackés.

L'objectif est qu'un reviewer **détecte ce pattern par lui-même** et finisse par approuver — pas qu'on lui dise "à l'itération 5+, sois plus souple". Cette dernière instruction biaiserait le verdict (le reviewer pourrait sur-approuver pour satisfaire l'instruction, indépendamment de la qualité réelle du code). Le but est l'auto-régulation par observation, pas la prescription.

## Le curseur

Le compromis fondamental :

- **Trop souple** → faux positif d'approbation, code cassé livré en prod.
- **Trop strict** → boucles infinies, le workflow ne termine jamais et brûle le budget.

Aucun prompt seul ne résout le compromis. Les leviers actuels :

| Levier | Effet |
|---|---|
| `confidence: low` traité comme soft-approval pour le routing fix (pas pour `stop`) | Évite qu'un doute fasse boucler le fixer |
| `stop` strict (deux `approved=true` cross-family) | Évite que la chaîne low/low termine le run |
| `pushback` du fixer + `prior_pushback` au reviewer suivant | Empêche un faux-positif persistant de bloquer la convergence |
| `previous_scanned_areas` | Pousse à élargir la couverture iter après iter, plutôt que rebattre les mêmes fichiers |
| `review_loop(N)` | Borne supérieure pour garantir terminaison |

## Pistes de prompt engineering ouvertes

1. **Auto-calibrage par tendance** : l'idée est que le reviewer compare sa propre densité de blockers à celle des itérations précédentes (via le verdict relayé). Si ses blockers ressemblent fortement à ceux déjà traités, il devrait abaisser sa barre. À tester en passant `loop.review_loop.previous_output.history` de manière concise — sans l'instruction explicite "approuve à l'iter N+", qui biaise.
2. **Cumulative scanned_areas** : actuellement on ne passe que la dernière itération via `loop.previous_output.scanned_areas`. Une vraie accumulation (union des `scanned_areas` de toutes les itérations) demanderait soit un opérateur d'union dans l'expression engine d'iterion, soit un pattern compute + history. À explorer.
3. **Blocker quality scoring** : ajouter un champ `blocker_severity_count` (count des blockers par sévérité critique/important/info) plutôt que blocker plat. Le streak_check pourrait alors se déclencher sur "0 critical depuis 2 iters" plutôt que sur `approved=true` pur.
4. **Anti-perfectionnisme côté fixer** : le fixer pourrait pushback plus agressivement si un blocker ressemble à du polish. Aujourd'hui le pushback est sous-utilisé.
5. **Ressemblance avec reviewer manuel** : observer si la séquence Claude→GPT→Claude→GPT introduit un biais (Claude plus rigoureux, GPT plus pragmatique). Tester aussi GPT-only et Claude-only en alternance avec des prompts variés.

## Observations empiriques de cette session

Lors de la première vraie validation (workflow durci, 30 avril 2026) :

- **Run 1** : 3 itérations review_loop. Bugs réels trouvés (claw `recovery.go` WorkDir guard, iterion `resume.go` vars re-seed). Sortie prématurée par hallucination GPT (`family: "missing-patch"` → fallback `done`). Fix : enum sur `family` + fallback `streak_check -> alt`.
- **Run 2** : 1 itération review_loop, convergence cross-family en ~5min. Reviewers focalisés sur les commits récents (via `git log` / `git show`), pas sur le code global. **Pas une convergence "naturelle" — une convergence par sous-périmètre.** Fix : prompts `review_system` étendus pour exiger un audit production-ready global et tracer `scanned_areas`.
- **Run 3** (à venir avec ce commit) : objectif est de voir si l'élargissement de scope produit plus d'itérations avec décroissance de blockers, ou s'il manque encore des leviers.

## Chemin du fichier de plan companion

Pour les sessions de refinement multi-jours, garder ici :

- Décisions de design et le pourquoi de chaque choix (session modes, loop bounds)
- Données empiriques par run (durée, nb itérations, coût, longest stretch sans intervention humaine)
- Adaptation guide quand on réutilise le pattern pour un autre projet

Voir [SKILL-run-and-refine.md](../SKILL-run-and-refine.md) pour la pratique générale du run/refine sur n'importe quel `.iter`.
