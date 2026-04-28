# claw-code-go ↔ Claude Code parity — état & reprise

État de la porte Rust (`/workspaces/iterion/.works/claw-code/`) → Go (`/workspaces/iterion/.works/claw-code-go/`) avec intégration iterion. Ce doc est l'état de référence pour reprendre la session.

Date d'arrêt : 2026-04-28 ~16h10 UTC.

---

## Commits livrés (à push si pas déjà fait)

### claw-code-go (master)
| SHA | Message | Status |
|---|---|---|
| `7799a7e` | fix(api): make Property recursive (items/enum/properties) | pushed |
| `bf21311` | feat(api): expose built-in tools via pkg/api/tools/ | pushed |
| `2574d7f` | feat(api): typed APIError on non-2xx (openai) | **NOT pushed** |

### iterion (main)
| SHA | Message | Status |
|---|---|---|
| `220f67a` | fix(live-tests): wire model field, codex/opus drift, Opus 4.7 + GPT 5.5 | **NOT pushed** |
| `bae0751` | test(live): session-inherit validation | **NOT pushed** |
| `e2af16b` | test(live): claw comprehensive coverage + provider-prefix bug | **NOT pushed** |
| `cbc1115` | feat(claw): registerable built-in tools, reasoning_effort observability, cache marshal contract | **NOT pushed** |
| `980ef58` | test(live): MCP integration end-to-end via claw + stdio server | **NOT pushed** |
| `493af5a` | test(live): long-context end-to-end via claw + Anthropic Haiku 4.5 | **NOT pushed** |
| `28aa810` | feat(model): retry on claw APIError + fix Anthropic prompt cache observability | **NOT pushed** |

> ⚠ Avant reprise : `cd .works/claw-code-go && git push origin master` puis `cd /workspaces/iterion && git push origin main`. Si le push claw-code-go inclut des commits postérieurs, retirer le `replace` de [`go.mod`](../go.mod) et `go get -u github.com/SocialGouv/claw-code-go@latest && go mod vendor`.

---

## Phases LIVRÉES (4/6 du plan + 3 fixes structurels)

| Phase | Description | Live test |
|---|---|---|
| 1 | Built-in tools library (read_file, write_file, bash, glob, grep, file_edit, web_fetch) wired via `tool.RegisterClawBuiltins(reg, workspace)` | `task test:live:claw-builtin-tools` PASS |
| 2.1 | reasoning_effort propagation jusqu'à `EventLLMRequest.data["reasoning_effort"]` | `task test:live:claw-reasoning` PASS |
| 2.2 | Retry classification — claw openai retourne `*api.APIError` typé sur 429/5xx + iterion `isRetryable` détecte les deux types d'erreur | unit test `TestClawBackend_RetryClassification` 4 sous-tests PASS |
| 2.3 | Anthropic prompt cache `cache_read_tokens > 0` validé end-to-end (cache_warm 18459 tokens écrits → cache_hit 18459 lus) | `task test:live:claw` (Phase 5 du workflow) PASS |
| 3 | MCP stdio server externe découvert + tools appelés par agent claw | `task test:live:claw-mcp` PASS |
| 4 | Long-context — 68 825 tokens shippés sans truncation, marker SHA echoé | `task test:live:claw-long-context` PASS |

Bonus : 4 live tests existants (full / review / kanban / session-inherit) toujours verts.

## Phases RESTANTES

| Phase | Description | Effort estimé | Impact |
|---|---|---|---|
| **2.1bis** | claw openai provider — route `/v1/responses` quand reasoning_effort + tools (sinon OpenAI 400 sur gpt-5.5+) | 2-3h | Edge case, peu fréquent |
| **5a** | Bedrock provider GA — implementer via `aws-sdk-go-v2 bedrock-runtime` à `internal/api/providers/bedrock/provider.go:6` (TODO actuel) | 4-6h | High (clients AWS) |
| **5b** | Vertex provider GA — implementer via `google.golang.org/api/option` à `internal/api/providers/vertex/provider.go:6` | 4-6h | Medium (clients GCP) |
| **5c** | Foundry provider GA — implementer via `github.com/Azure/azure-sdk-for-go` à `internal/api/providers/foundry/provider.go:7` | 4-6h | Low (Azure niche) |
| **6** | Lifecycle hooks (UserPromptSubmit, PreToolUse, PostToolUse, PreCompact). Rust ref : `.works/claw-code/rust/crates/plugins/src/hooks.rs` (564 LOC). Cible Go : nouveau package `internal/hooks/runner.go` | 6-8h | Medium-High |
| **7** | Permission modes auto / dontAsk. Mode `auto` requiert un classifier model serveur-side. Mode `dontAsk` est plus simple — strict allow-list. Code existant : `internal/permissions/` | 2-3h | Medium |

Total restant : ~25-35h cumulé.

---

## Phase 2.1bis — état du travail en cours (interrompu)

Décision prise au moment de l'arrêt : ajouter une méthode `streamResponses` dans [`.works/claw-code-go/internal/api/providers/openai/provider.go`](../../.works/claw-code-go/internal/api/providers/openai/provider.go) qui parle l'API OpenAI Responses (`POST /v1/responses`). Détection de l'aiguillage : `req.ReasoningEffort != "" && len(req.Tools) > 0 && isReasoningModel(req.Model)`.

### Spec OpenAI Responses (à implémenter)

**Endpoint** : `POST /v1/responses`

**Body shape** :
```json
{
  "model": "gpt-5.5",
  "instructions": "<system prompt>",
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "..."}]}],
  "tools": [{"type": "function", "name": "...", "description": "...", "parameters": {...}}],
  "reasoning": {"effort": "high"},
  "tool_choice": {"type": "function", "name": "..."},
  "stream": true
}
```

**Stream events** (différents de `/v1/chat/completions`) :
- `response.created` — initial response object
- `response.output_item.added` — new output item (text or function call)
- `response.output_text.delta` — incremental text content
- `response.function_call_arguments.delta` — incremental function args
- `response.function_call_arguments.done` — function call complete
- `response.completed` — final response with usage

### Plan d'implémentation

1. Définir le wire format dans [`provider.go`](../../.works/claw-code-go/internal/api/providers/openai/provider.go) (struct `oaiResponsesRequest`, `oaiResponsesEvent`)
2. Ajouter `func (c *Client) streamResponses(ctx, req)` qui :
   - Marshal vers `/v1/responses` body
   - POST, gère 4xx via `*api.APIError` (déjà câblé)
   - Lit le SSE stream, traduit chaque event en `api.StreamEvent` Anthropic-style (le reste de l'agrégation amont attend ce format)
3. Patcher `StreamResponse` (ligne 204) : si reasoning+tools, dispatch vers `streamResponses`, sinon laisser le path actuel `/v1/chat/completions`
4. Mettre à jour le live test [`examples/claw_reasoning_effort.iter`](../examples/claw_reasoning_effort.iter) pour ajouter un tool synthétique sur le node `thinker` ; assertion : `EventNodeFinished.output` est rempli (preuve que `/v1/responses` retourne du contenu utilisable)
5. Build + run

### Code mort-né laissé en mémoire

- `.works/claw-code-go/internal/api/providers/openai/provider.go` : ajouts faits — helpers `extractOpenAIErrorMessage`, `truncateBody` (lignes 626+), `*api.APIError` retourné sur non-200 (lignes 230+). C'est commité dans `2574d7f`.
- iterion : `model/executor.go:734-762` détecte les deux types d'`APIError`. Commit `28aa810`.
- Aucun code WIP non-commité au moment de l'arrêt.

---

## Phase 5 — providers cloud (notes d'implémentation)

### 5a — Bedrock

Stub : [`.works/claw-code-go/internal/api/providers/bedrock/provider.go:6`](../../.works/claw-code-go/internal/api/providers/bedrock/provider.go) `// TODO: implement using aws-sdk-go-v2 bedrock-runtime`.

Approche : utiliser `github.com/aws/aws-sdk-go-v2/service/bedrockruntime`. L'API supporte le streaming via `InvokeModelWithResponseStream`. Modèles Anthropic disponibles via Bedrock : `anthropic.claude-3-5-sonnet-*`, etc. Le format de message est Anthropic-style mais l'auth est SigV4. Pas besoin de réécrire la logique d'agrégation — le format de payload Bedrock pour modèles Anthropic est presque identique à l'API directe.

Ressources de la version Rust à porter : `/workspaces/iterion/.works/claw-code/rust/crates/api/src/providers/bedrock/` (à confirmer existence).

### 5b — Vertex

Stub : [`.works/claw-code-go/internal/api/providers/vertex/provider.go:6`](../../.works/claw-code-go/internal/api/providers/vertex/provider.go).

Approche : utiliser `google.golang.org/api/aiplatform/v1` ou la lib `cloud.google.com/go/aiplatform`. Auth via Google ADC (Application Default Credentials). L'API Vertex pour Anthropic models : `https://us-east5-aiplatform.googleapis.com/v1/projects/{project}/locations/{loc}/publishers/anthropic/models/{model}:streamRawPredict`. Format payload Anthropic-compatible.

### 5c — Foundry

Stub : [`.works/claw-code-go/internal/api/providers/foundry/provider.go:7`](../../.works/claw-code-go/internal/api/providers/foundry/provider.go).

Azure OpenAI Service ; auth via API key ou Azure AD. SDK `github.com/Azure/azure-sdk-for-go/sdk/ai/azopenai`.

---

## Phase 6 — Lifecycle hooks

Rust ref : `.works/claw-code/rust/crates/plugins/src/hooks.rs` (564 LOC) + `lib.rs` (3657 LOC).

Architecture cible côté Go :
- Nouveau package `internal/hooks/`
- Types : `HookEvent` (PreToolUse, PostToolUse, PostToolUseFailure, UserPromptSubmit, PreCompact, PostCompact), `HookHandler`, `HookRunner`
- Hook handlers exécutent des commandes shell ou des fonctions Go enregistrées
- Wiring dans `runtime/conversation.go` aux points de tool exec, prompt submit, compaction

Risque : changements structurels dans `internal/runtime/` sont étendus. Tester avec un harness mock.

## Phase 7 — Permission modes auto/dontAsk

Modes existants dans [`.works/claw-code-go/internal/permissions/`](../../.works/claw-code-go/internal/permissions) : `ModeAllow`, `ModePrompt`, `ModeReadOnly`, `ModeWorkspaceWrite`, `ModeDangerFullAccess`.

À ajouter :
- `ModeDontAsk` — strict allow-list. Une opération non-listée → refus immédiat sans prompt. Petit ajout (~50 lignes).
- `ModeAuto` — un classifier LLM décide. Plus complexe, requiert appel LLM séparé. Probablement à reporter sur une session ultérieure.

---

## Stratégie de reprise

### Option A — reprise manuelle dans une nouvelle session Claude Code
1. Lire ce fichier en premier
2. `cd /workspaces/iterion && git status` puis `git push origin main` si non-pushé
3. `cd .works/claw-code-go && git push origin master` si non-pushé (le `replace` dans iterion go.mod garde tout fonctionnel localement)
4. Ouvrir le todo state ci-dessous et reprendre depuis Phase 2.1bis

### Option B — déléguer à iterion (méta-iteration)
Écrire un workflow `.iter` qui orchestre la suite via claude_code/claw agents :
- Phase par phase, avec checkpoints à chaque commit
- Le prompt système contient ce doc
- Workflow loop sur les phases restantes jusqu'à exhaustion

**Bonus** : c'est exactement le use-case d'iterion. Le faire serait éloquent (iterion s'auto-développe).

Architecture proposée :
- 1 agent par phase (claude_plan + claude_implement avec session: inherit)
- 1 judge final qui vérifie les builds + tests + commits faits
- LLM router pour passer à la phase suivante

À drafter dans une 3e session.

---

## Snapshot todo (au moment de l'arrêt)

```
[completed] Phase 1.1-1.4: built-in tools wired and live-tested
[completed] Phase 2.1: reasoning_effort live test
[completed] Phase 3: MCP live integration test
[completed] Phase 4: Long-context live test
[completed] Phase 2.2: claw-code-go openai provider — return structured APIError on 429/5xx
[completed] Phase 2.2 unit test: verify retry fires on *api.APIError
[completed] Phase 2.3: cache_read — root cause = Claude 4 threshold ~3k tokens; fix = bigger prompt
[in_progress] Phase 2.1bis: claw-code-go openai provider — route to /v1/responses for reasoning+tools
[pending] Phase 5: Bedrock/Vertex/Foundry provider implementations (stubs → GA)
[pending] Phase 6: Lifecycle hooks (UserPromptSubmit, PreCompact, ToolUse pre-execution)
[pending] Phase 7: Permission modes auto / dontAsk
```

## Pointeurs clés

- Plan original : [`/home/jo/.claude/plans/on-a-fait-pas-reflective-papert.md`](/home/jo/.claude/plans/on-a-fait-pas-reflective-papert.md)
- Audit Claude Code vs claw-code-go (résultats résumés en haut, agent task `a0bfe38073d7f02f6` dans la session)
- Audit Rust → Go parity (résultats résumés en haut, agent task `a28af97d9c5851303`)
- Live runs (artifacts pour comparaison) : [`.live-runs/COMPARISON.md`](COMPARISON.md)
