# Fixtures de référence iterion V1

Ce document décrit le rôle de chaque fixture de référence, leur relation et les primitives V1 qu'elles exercent.

## Vue d'ensemble

| Fixture | Objectif | Modèles | Parallélisme | Human | Boucles |
|---|---|---|---|---|---|
| `pr_refine_single_model` | Baseline single-model | 1 | non | non | refine(4) + recipe(3) |
| `pr_refine_dual_model_parallel` | Dual-model sans compliance gate | 2 | oui (fan-out) | non | recipe(3) |
| `pr_refine_dual_model_parallel_compliance` | **Workflow phare V1** | 2+ | oui (fan-out) | oui | refine(6) + recipe(3) |
| `recipe_benchmark` | Comparaison de recettes | N | oui (fan-out) | non | aucune |
| `ci_fix_until_green` | Fix CI itératif | 1 | non | non | fix(5) |
| `pr_refine_dual_model_parallel_mcp` | Variante MCP (delegation) | 2 | oui (fan-out) | non | recipe(3) |

## Détail des fixtures

### `pr_refine_single_model`

**Chemin nominal :** context → review → plan → compliance → act → verify → done/reboucle.

Un seul modèle parcourt tout le workflow. Sert de **baseline de coût et de qualité** pour comparer avec les variantes multi-modèles. Exerce les primitives fondamentales : agent, judge, boucle bornée, publish, session fresh et inherit.

### `pr_refine_dual_model_parallel`

**Chemin nominal :** context → [claude_review | gpt_review] → [claude_plan | gpt_plan] → join → [claude_synthesis | gpt_synthesis] → join → merge → act → [claude_final | gpt_final] → join → verdict → done/reboucle.

Variante allégée du workflow phare. Deux modèles en parallèle, synthèse croisée, merge, act, review finale parallèle. **Pas de compliance gate intermédiaire ni de gate humain.** Exerce : router fan_out_all, join wait_all, multi-modèles.

### `pr_refine_dual_model_parallel_compliance`

**Workflow phare de la V1.** Chemin nominal identique à `pr_refine_dual_model_parallel` mais avec :
- Judge de compliance après le merge du plan
- Gate humain optionnel pour arbitrage technique
- Boucle de raffinage alternée Claude/GPT (max 6 itérations)
- Recheck compliance après intégration des clarifications humaines

Exerce **toutes les primitives V1** : agent, judge, router, join, human, done, fail, boucle locale, reloop global, publish, session fresh/inherit/artifacts_only, multi-modèles, tools, budgets.

### `recipe_benchmark`

**Chemin nominal :** orchestrator → [recipe_a | recipe_b] → join → judge → done.

Exécute deux recettes en parallèle sur la même PR, agrège les résultats, compare via un judge. Sert à **comparer coût, qualité, itérations et latence** entre recettes. Extensible à N recettes.

### `pr_refine_dual_model_parallel_mcp`

**Chemin nominal :** identique a `pr_refine_dual_model_parallel`.

Variante MCP du workflow dual-model parallele. Au lieu d'appeler les APIs LLM directement (`model:`), chaque noeud delegue son travail a un agent CLI externe (`delegate:`). Les noeuds Claude utilisent `claude_code` (claude-code CLI), les noeuds GPT utilisent `codex` (OpenAI Codex CLI). Le graphe, schemas, prompts et edges sont identiques a la version API. Exerce la primitive `delegate` en plus de router, join, publish, boucle. Voir `docs/mcp_delegation.md` pour la comparaison detaillee API vs delegation.

### `ci_fix_until_green`

**Chemin nominal :** diagnose → plan → act → run_ci → verify → done ou reboucle.

Pattern itératif de correction CI. Diagnostique l'échec, planifie un fix, applique, relance la CI, vérifie. Reboucle jusqu'à CI vert (max 5 itérations). Exerce le nœud `tool` (exécution directe de commande sans LLM) et les boucles sur l'intégralité du workflow.

## Relations entre fixtures

```
pr_refine_single_model          ← baseline simple, 1 modèle
    ↓ (ajouter parallélisme)
pr_refine_dual_model_parallel   ← dual-model, pas de gate
    ↓ (delegation MCP)
pr_refine_dual_model_parallel_mcp        ← variante delegation (claude-code + codex)
    ↓ (ajouter compliance + human)
pr_refine_dual_model_parallel_compliance  ← workflow phare complet
    ↓ (benchmarker)
recipe_benchmark                ← comparer des variantes

ci_fix_until_green              ← pattern indépendant (CI, pas PR)
```

## Usage

Chaque fixture est conçue pour être exploitable dans trois contextes :
1. **Tests** — parseable et compilable vers IR, sert de cas de test pour parser (P1), compilateur (P2) et runtime (P3+).
2. **Documentation produit** — lisible comme spécification, avec commentaires inline décrivant le chemin nominal.
3. **Rendu Mermaid** — compilable vers un diagramme de workflow pour visualisation (P8-02).
