# claw-code-go ↔ Claude Code parity — état & reprise

État de la porte Rust (`/workspaces/iterion/.works/claw-code/`) → Go (`/workspaces/iterion/.works/claw-code-go/`) avec intégration iterion. Ce doc est l'état de référence pour reprendre la session.

Date de complétion : 2026-04-28 ~17h UTC. **Toutes les phases initialement identifiées sont livrées.**

---

## Commits livrés (à push si pas déjà fait)

### claw-code-go (master) — 6 commits en avance d'origin

| SHA | Message |
|---|---|
| `7799a7e` | fix(api): make Property recursive (items/enum/properties) |
| `bf21311` | feat(api): expose built-in tools via pkg/api/tools/ |
| `2574d7f` | feat(api): typed APIError on non-2xx (openai) |
| `14716b8` | feat(openai): route reasoning_effort+tools to /v1/responses |
| `3ce3cea` | feat(bedrock): real AWS Bedrock provider with aws-sdk-go-v2 |
| `4f6a013` | feat(vertex,foundry): real Vertex AI and Azure Foundry providers |
| `6f29983` | feat(permissions): add ModeDontAsk and ModeAuto with Classifier interface |
| `bd616bf` | feat(hooks): in-process lifecycle Runner + integration in conversation.go |

### iterion (main) — N commits en avance d'origin

| SHA | Message |
|---|---|
| `220f67a` | fix(live-tests): wire model field, codex/opus drift, Opus 4.7 + GPT 5.5 |
| `bae0751` | test(live): session-inherit validation |
| `e2af16b` | test(live): claw comprehensive coverage + provider-prefix bug |
| `cbc1115` | feat(claw): registerable built-in tools, reasoning_effort observability, cache marshal contract |
| `980ef58` | test(live): MCP integration end-to-end via claw + stdio server |
| `493af5a` | test(live): long-context end-to-end via claw + Anthropic Haiku 4.5 |
| `28aa810` | feat(model): retry on claw APIError + fix Anthropic prompt cache observability |
| `8303b01` | docs(port): claw parity progress + resume plan |
| `4824d3d` | feat(model): wire bedrock/vertex/foundry provider factories |

> ⚠ Avant push : `cd .works/claw-code-go && git push origin master` PUIS `cd /workspaces/iterion && git push origin main`. Une fois claw poussé, retirer le `replace github.com/SocialGouv/claw-code-go => ./.works/claw-code-go` de [`go.mod`](../go.mod) et `go get github.com/SocialGouv/claw-code-go@latest && go mod tidy && go mod vendor`. Commit final cleanup.

---

## Phases LIVRÉES (toutes)

| Phase | Description | Couverture |
|---|---|---|
| 1 | Built-in tools library (read_file, write_file, bash, glob, grep, file_edit, web_fetch) wired via `tool.RegisterClawBuiltins(reg, workspace)` | `task test:live:claw-builtin-tools` PASS |
| 2.1 | reasoning_effort propagation jusqu'à `EventLLMRequest.data["reasoning_effort"]` | `task test:live:claw-reasoning` PASS |
| 2.1bis | OpenAI `/v1/responses` routing quand reasoning_effort + tools (Phase 2.1 unblocked pour gpt-5.5+) | `responses_test.go` httptest SSE PASS |
| 2.2 | Retry classification — claw openai retourne `*api.APIError` typé sur 429/5xx + iterion `isRetryable` détecte les deux types d'erreur | `TestClawBackend_RetryClassification` 4 sous-tests PASS |
| 2.3 | Anthropic prompt cache `cache_read_tokens > 0` validé end-to-end (cache_warm 18459 tokens écrits → cache_hit 18459 lus). Root cause = seuil ~3k tokens pour Claude 4-series (vs 1024 pour Claude 3.x) | `task test:live:claw` (Phase 5 du workflow) PASS |
| 3 | MCP stdio server externe découvert + tools appelés par agent claw | `task test:live:claw-mcp` PASS |
| 4 | Long-context — 68 825 tokens shippés sans truncation, marker SHA echoé | `task test:live:claw-long-context` PASS |
| 5a | AWS Bedrock provider GA via `aws-sdk-go-v2 bedrockruntime` (Anthropic models). Standard SDK credential chain. APIError mapping pour Throttling/Validation/AccessDenied/etc. | 14 unit tests PASS |
| 5b | GCP Vertex AI provider GA via Google ADC (oauth2/google). `streamRawPredict` endpoint Anthropic-compatible | 7 unit tests PASS |
| 5c | Azure Foundry (Azure OpenAI Service) provider GA via `azidentity` ou `AZURE_OPENAI_API_KEY`. Chat Completions wire | 9 unit tests PASS |
| 6 | Lifecycle hooks in-process (PreToolUse, PostToolUse, PostToolUseFailure, UserPromptSubmit, PreCompact, PostCompact, Stop). Block/Modify/Continue decisions, panic recovery, race-clean | 8 runner + 3 integration tests PASS |
| 7 | Permission modes ModeDontAsk (strict allow-list) + ModeAuto (Classifier interface, default RuleClassifier safe-list pour read_file/glob/grep/web_fetch HTTPS) | 12 unit tests PASS |

**Bonus** : 4 live tests existants (full / review / kanban / session-inherit) toujours verts. iterion expose désormais via `model.Registry` les 5 providers : `anthropic/`, `openai/`, `bedrock/`, `vertex/`, `foundry/`.

## Suite optionnelle (hors scope initial)

L'audit Rust→Go a aussi identifié des écarts non couverts par cette session :
- **Plugin system complet** (lifecycle, marketplace, on-disk hook config) — la version Rust fait ~3.6K LOC. Le `Runner` du Phase 6 est l'intégration point pour cela.
- **Telemetry OTLP** approfondie (le code Go est ~36 LOC, le Rust ~526 LOC).
- **Worker boot depth** — le runtime Go simplifie certaines branches du runtime Rust.
- **Recovery recipes** par classe d'erreur.
- **Trust resolver** completion.
- **PowerShell support** sur bash tool (Windows).
- **MCP OAuth broker** pour MCP distant authentifié.

Aucun de ces items ne bloque l'usage normal. Ils peuvent faire l'objet de phases ultérieures séparées.

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
