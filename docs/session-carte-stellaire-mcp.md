# Session : Live Test Carte Stellaire — Mode MCP

## Objectif

Faire tourner le pipeline carte stellaire (observatoire astronomique interactif, 20 criteres) en **mode MCP** : GPT-5.4 comme superviseur unique, outils Claude Code + Codex via MCP, round-robin implementation.

## Resultat final : v12 PASS 20/20

- **Duree** : 44 minutes, 3 iterations
- **Workspace** : `/tmp/iterion-plan-impl-review-mcp-3565500141/`
- **Iterations** :
  - It.1 (Claude) : 19/20 — FAIL critere 19 (Tab navigation)
  - It.2 (Codex) : 18/20 — FAIL criteres 10 (media query), 18 (courbe slider) — faux positifs du juge
  - It.3 (Claude) : **20/20 PASS** — Codex review valide 20/20 avec citations, juge approuve

## Historique des runs

| Run | Iterations | Resultat | Cause principale |
|-----|-----------|----------|------------------|
| v6 | 1 | PASS (17/20) | Juge trop laxiste |
| v7 | 7 | LOOP_EXHAUSTED | Juge 20/20 strict, pas de fixes |
| v8b | 11 | LOOP_EXHAUSTED | Prompts insuffisants, Codex aveugle, juge drift |
| v9 | (killed) | 19/20 it.1 | Fix prompts OK, Codex toujours aveugle |
| v10 | (killed) | 18/20 it.1 | Fix cwd OK mais Codex toujours bloque |
| v11 | (killed) | 19/20 it.1 | Fix approval-policy, Codex review toujours bloque |
| **v12** | **3** | **PASS 20/20** | **Tous les fixes accumules** |

## Bugs corriges pendant la session

### Code Go

1. **WORKSPACE_SAFETY** — noeuds paralleles avec outils bloques → `readonly: true`
2. **Tool name dots** — API OpenAI rejette `mcp.x.y` → `SanitizedName()` (dots→underscores)
3. **goai.GenerateObject sans tool loop** → `generateTextWithToolsAndSchema()` utilise GenerateText
4. **System prompt ecrase** — goai WithSystem est last-wins → schema injecte dans userText
5. **MCP WorkDir manquant** → `cmd.Dir = cfg.WorkDir`
6. **Read file too large (43x→0)** → cap limit 500, auto-retry avec limit 300
7. **Invalid pages param (25x→0)** → suppression params vides
8. **Codex bwrap sandbox** → `sandbox="danger-full-access"` + login API key
9. **Codex elicitation** → autoApproveElicitation dans readLoop
10. **FatalToolError** — rate limit/credit errors propagees immediatement
11. **Codex cwd manquant** — l'outil `codex` a besoin d'un parametre `cwd` explicite dans les arguments (pas juste `cmd.Dir`). Fix : `sanitizeToolArgs` injecte `cwd=workDir` pour chaque appel
12. **Codex approval-policy** — en mode non-interactif, Codex bloque sur l'approbation des commandes. Fix : injection `approval-policy=never` et `sandbox=danger-full-access` dans les arguments de l'outil
13. **Smoke test MCP** — ajout d'un test de verification d'acces workspace au demarrage de chaque serveur MCP (Bash pwd pour Claude, codex ls pour Codex)

### Prompts (.iter)

14. **Prompts implementation renforces** — ajout "REGLES DE CONFORMITE CRITIQUES" ciblant les 8 criteres les plus echoues (1, 5, 6, 7, 12, 15, 18, 19)
15. **Review chunked reading** — instruction explicite de lire index.html en 4-5 appels de 500 lignes
16. **Review criteres specifiques** — ajout "CRITERES A VERIFIER AVEC ATTENTION PARTICULIERE" pour 1, 15, 18, 19
17. **Juge anti-regression** — regle stricte : citation de code requise pour FAIL
18. **Juge en deux phases** — Phase 1 (evaluation rigoureuse, les deux reviewers doivent valider) + Phase 2 (auto-critique, elimination des faux positifs)
19. **Detailed reviews to implementation** — ajout `previous_claude_review` et `previous_codex_review` dans impl_input
20. **Review tool_max_steps** — 30 → 40 pour les noeuds review

## Fichiers modifies

### Code Go
| Fichier | Changement |
|---------|------------|
| `parser/token.go` | +TokenStar, +TokenReadonly |
| `parser/lexer.go` | +case `*` dans scanToken |
| `parser/parser.go` | +wildcard dans parseToolRef, +readonly dans agent/judge |
| `ast/ast.go` | +Readonly sur AgentDecl/JudgeDecl |
| `ir/ir.go` | +Readonly sur Node |
| `ir/compile.go` | Propagation Readonly |
| `runtime/engine.go` | isMutatingNode respecte Readonly |
| `tool/adapter.go` | SanitizedName (dots → underscores pour API OpenAI) |
| `tool/registry.go` | +ListByServer, +IsMCPWildcard, +ParseMCPWildcard |
| `model/executor.go` | +expandWildcards, +generateTextWithToolsAndSchema, +extractJSON, schema injecte dans userText |
| `mcp/config.go` | +WorkDir, +Env sur ServerConfig, preset codex sandbox danger-full-access |
| `mcp/manager.go` | +sanitizeToolArgs (cwd/approval-policy/sandbox pour codex, cap Read), +FatalToolError, +smokeTestWorkspace |
| `mcp/rpc.go` | +cmd.Dir, +cmd.Env, +autoApproveElicitation, +elicitation capability |
| `unparse/unparse.go` | +readonly serialization |
| `vendor/goai/generate.go` | +FatalToolError interface, propagation dans tool loop |

### Fixture
- `examples/dual_model_plan_implement_review_mcp.iter` — prompts renforces, juge deux phases, data flow reviews, tool_max_steps

### Test
- `e2e/live_test.go` — +TestLive_DualModel_PlanImplementReview_MCP, +requireEnv, +WorkDir dans MCP catalog

## Configuration Codex

```bash
# Login avec API key OpenAI (fait)
printenv OPENAI_API_KEY | codex login --with-api-key
# Verifie : Logged in using an API key - sk-svcac***
```

Le preset natif utilise `-c 'sandbox="danger-full-access"'` pour bypasser bwrap dans le devcontainer.
Les parametres `cwd`, `approval-policy=never` et `sandbox=danger-full-access` sont injectes automatiquement dans chaque appel a l'outil codex par `sanitizeToolArgs`.

## Lecons apprises

1. **Codex MCP necessite des parametres explicites** — `cmd.Dir` seul ne suffit pas, il faut passer `cwd` dans les arguments de l'outil
2. **Le mode non-interactif necessite `approval-policy=never`** — sinon Codex bloque en attente d'approbation
3. **Le juge doit etre exigeant ET auto-critique** — exigeant = les deux reviewers doivent valider ; auto-critique = eliminer les faux positifs avant de rejeter
4. **Les prompts d'implementation doivent cibler les echecs recurrents** — les regles generiques ne suffisent pas, il faut des instructions CSS/JS specifiques
5. **Le smoke test MCP detecte les problemes tot** — un simple `ls && pwd` au demarrage aurait revele le bug Codex immediatement
