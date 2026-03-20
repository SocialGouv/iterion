# Périmètre V1 — Grammaire & AST iterion

## Primitives couvertes par la grammaire et l'AST V1

| Primitive | Mot-clé DSL | Nœud AST | Notes |
|-----------|-------------|----------|-------|
| Variables | `vars:` | `VarsBlock`, `VarField` | Top-level et workflow-level |
| Prompts | `prompt <name>:` | `PromptDecl` | Texte libre avec `{{...}}` |
| Schemas | `schema <name>:` | `SchemaDecl`, `SchemaField` | Types : string, bool, int, float, json, string[] ; contrainte enum |
| Agent | `agent <name>:` | `AgentDecl` | model, input, output, publish, system, user, session, tools, tool_max_steps |
| Judge | `judge <name>:` | `JudgeDecl` | Identique structurellement à agent |
| Router | `router <name>:` | `RouterDecl` | Modes : fan_out_all, condition |
| Join | `join <name>:` | `JoinDecl` | Stratégies : wait_all, best_effort ; require, output |
| Human | `human <name>:` | `HumanDecl` | input, output, publish, instructions, mode, min_answers |
| Tool (nœud) | `tool <name>:` | `ToolNodeDecl` | command, output (exécution directe sans LLM) |
| done / fail | (réservés) | Cibles d'edges | Pas de déclaration, reconnus par le parser |
| Workflow | `workflow <name>:` | `WorkflowDecl` | vars, entry, budget, edges |
| Budget | `budget:` | `BudgetBlock` | max_parallel_branches, max_duration, max_cost_usd, max_tokens, max_iterations |
| Edge | `src -> dst` | `Edge` | with, when, as (loop) |
| When | `when [not] <cond>` | `WhenClause` | Condition + négation |
| Loop | `as <name>(<N>)` | `LoopClause` | Boucle nommée et bornée |
| With | `with { ... }` | `WithEntry` | Mapping données inter-nœuds |
| Session | `session:` | `SessionMode` | fresh, inherit, artifacts_only |
| Publish | `publish:` | Champ sur Agent/Judge/Human | Artefact persistant |
| Template | `{{...}}` | Dans les valeurs string | vars.X, input.X, outputs.X[.Y], artifacts.X |
| Env refs | `${...}` | Dans les valeurs string | Résolution runtime |
| Commentaires | `## ...` | `Comment` | Dans le fichier et dans le workflow |

## Explicitement hors V1

| Concept | Raison |
|---------|--------|
| **Imports / includes** | Un fichier = un workflow. Pas de système de modules en V1. |
| **Héritage de nœuds** | Pas de `extends` ou de composition de nœuds. Duplication acceptable. |
| **Types composites dans schemas** | Pas de types imbriqués ou de `map`. `json` sert de type fourre-tout. |
| **Expressions conditionnelles complexes** | `when` prend un identifiant simple, pas d'expressions booléennes composées (&&, ||). |
| **Router mode: condition avec expressions** | Le mode `condition` est déclaré mais les règles de routage conditionnelles complexes sont hors V1. |
| **Sous-workflows / appels de workflow** | Un workflow ne peut pas en appeler un autre. |
| **Retry / backoff sur nœuds** | Géré au niveau runtime/policy, pas dans le DSL. |
| **Timeouts par nœud** | Seul le budget global `max_duration` est supporté en V1. |
| **Variables dynamiques** | Les vars sont déclarées statiquement ; pas de computed vars. |
| **Annotations / metadata** | Pas de système d'annotations libre sur les nœuds. |
| **Validation sémantique** | La grammaire et l'AST ne valident pas les références croisées ; cela relève de P2 (compilation AST → IR). |
| **Typage des templates** | Les `{{...}}` sont des strings opaques dans l'AST ; le typage est vérifié à la compilation. |
| **Multi-workflow par fichier** | Techniquement possible dans la grammaire (le champ `Workflows` est un slice), mais un seul workflow par fichier est la convention V1. |
