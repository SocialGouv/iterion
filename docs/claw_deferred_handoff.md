# Handoff — claw-code-go deferred features

Document complet permettant à un nouvel agent global de reprendre les 7 features différées de la session du 28 avril 2026. Se lit avant tout dispatch.

---

## TL;DR

Un sprint de port Rust→Go a livré l'essentiel (parité Claude Code passée de 70% à 85%). 7 morceaux ont été reportés parce que les subagents timeoutent au-delà de ~10 minutes sur des tâches lourdes (>500 LOC). Cette doc explique :
- l'état exact de chaque morceau différé (code déjà posé vs code manquant),
- la spec d'implémentation,
- les commandes de vérification,
- l'ordre d'attaque recommandé,
- les pièges du dispatch parallèle.

**Avant tout** : lire `docs/claw_port_progress.md` pour le contexte de la session précédente, et `docs/parity.md` pour la matrice de capacités à jour.

---

## Pré-requis de l'environnement

Repo : `/workspaces/iterion/.works/claw-code-go/` (claw-code-go) et `/workspaces/iterion/` (iterion).

iterion utilise un `replace github.com/SocialGouv/claw-code-go => ./.works/claw-code-go` dans `go.mod` tant que les commits récents de claw ne sont pas pushés sur origin. Vérifier `git -C .works/claw-code-go log origin/master..HEAD --oneline` avant tout pour savoir si le replace est encore nécessaire.

Build/test convention : tout passe par `devbox run --config=/workspaces/iterion -- go -C /workspaces/iterion/.works/claw-code-go ...` (le pwd de devbox reste `/workspaces/iterion`).

API keys disponibles dans `/workspaces/iterion/.env` : `ANTHROPIC_API_KEY` et `OPENAI_API_KEY`.

---

## Pièges du dispatch parallèle (à éviter)

La session précédente a dispatché 10 subagents en parallèle. **6 ont timeouté.** Diagnostic :

1. **Stream idle timeout ~10 min** sur les tâches lourdes (>500 LOC). Les subagents qui font de la recherche/délibération avant de coder consomment leur budget temps sans produire d'output.
2. **Conflits de stash** quand plusieurs subagents touchent les mêmes fichiers partagés (ex. `pkg/api/types.go`). L'un ouvre un stash, l'autre voit des modifs « apparues » et se coince.
3. **Ambiguïté sur l'env shell** : un subagent a perdu 10 min à chercher `go` sans utiliser `devbox run`.

**Règles pour éviter** :

- **1 feature = 1 subagent**, scope max ~250 LOC d'écriture (incluant tests).
- **Pas de parallèle sur des fichiers partagés** : si deux features touchent `internal/api/types.go`, faire séquentiellement.
- **Donner explicitement la commande de build** dans le prompt : `cd /workspaces/iterion/.works/claw-code-go && devbox run --config=/workspaces/iterion -- go -C $(pwd) build ./...`.
- **Préférer 3 subagents séquentiels** plutôt que 6 en parallèle pour les features qui partagent un package proche.

---

## État actuel (28 avril 2026, après wave 4)

claw-code-go a 14 commits depuis `bf21311` (origin/master). Liste raccourcie :

```
14716b8 feat(openai): /v1/responses routing pour reasoning+tools
3ce3cea feat(bedrock): real AWS Bedrock provider
4f6a013 feat(vertex,foundry): real Vertex AI and Azure Foundry providers
6f29983 feat(permissions): ModeDontAsk + ModeAuto + Classifier interface
bd616bf feat(hooks): in-process lifecycle Runner
f409e88 chore: gofmt -w
80f5a8c refactor+fix: extract httputil/sseutil/openaiwire + 6 bugs + 7 quality
52392bd feat(api): add ImageSource for vision content blocks
4a42881 feat(mcp): atomic disk-backed token storage (broker pending)
docs/bench commits...
```

iterion a 6 commits depuis `2209508`. Tree clean côté code, exemples + doc en place.

---

## 1. BUG 4 — hook lifecycle ctx propagation

### Contexte

Un agent précédent a démarré un fix qui change la signature de `internal/runtime/conversation.go::ExecuteTool` pour accepter un `ctx context.Context`. Il a modifié `internal/runtime/loop_prompt.go` pour appeler `ExecuteTool(ctx, name, args)` mais **n'a pas modifié la signature de `ExecuteTool`**, et n'a pas mis à jour les autres callers. Le fix de loop_prompt a été reverté pour ré-arrêter le build, donc à ce stade aucun changement n'est en place.

### Cible

`internal/runtime/conversation.go::ExecuteTool(name string, input map[string]any)` doit devenir `ExecuteTool(ctx context.Context, name string, input map[string]any)`. Tous les call sites doivent être mis à jour. Les helpers internes `fireLifecyclePreToolUse`, `fireLifecyclePostToolUse`, `tryCompact`, `fireUserPromptSubmit` qui passent actuellement `context.Background()` à `LifecycleHooks.Fire` doivent recevoir le ctx du caller.

### Plan d'attaque

1. `grep -rn "loop\.ExecuteTool\|conversationLoop\.ExecuteTool\|\.ExecuteTool(" .` pour lister tous les call sites.
2. Ajouter le param `ctx` à la définition.
3. Modifier les helpers Fire pour accepter ctx en paramètre.
4. Plumber ctx depuis `SendMessage` / `SendMessageStreaming` jusqu'à `ExecuteTool` (probablement via un champ sur `ConversationLoop` ou un nouveau param sur les helpers).

### Vérification

```bash
cd /workspaces/iterion/.works/claw-code-go
devbox run --config=/workspaces/iterion -- go -C $(pwd) build ./internal/runtime/...
devbox run --config=/workspaces/iterion -- go -C $(pwd) test -race -short ./internal/runtime/...
```

### Test à ajouter

`internal/runtime/conversation_test.go::TestExecuteTool_PropagatesCtxCancellation` — démarre un hook handler qui sleep, annule le ctx, vérifie que la cancellation est observable côté handler.

### Effort estimé

200-300 LOC, 1 subagent, ~5 min.

---

## 2. BUGs 2/3 — OpenAI Responses interleaved message items

### Contexte

`internal/api/providers/openai/responses.go::streamResponsesEvents` (lignes ~374-465) hardcode `textBlockIndex = 0`. Si le stream émet un second `response.output_item.added{type:"message"}` après un function call, tous les `response.output_text.delta` continuent à viser le bloc 0.

Note : BUG 7 (args avant id+name) est implicitement résolu par `sseutil.ToolCallAccumulator` (commit 80f5a8c). Reste BUG 2 + BUG 3 (qui sont la même chose : tracking par item_id).

### Cible

Convertir le tracking texte en `map[itemID]int` (block index par item_id), comme c'est déjà fait pour les function calls via `fnByItem`. Émettre `EventContentBlockStop` à la fin de chaque item de message + un nouveau `EventContentBlockStart` au début du suivant.

### Vérification

```bash
devbox run --config=/workspaces/iterion -- go -C /workspaces/iterion/.works/claw-code-go test -race -short ./internal/api/providers/openai/...
```

### Test à ajouter

`responses_test.go::TestStreamResponses_InterleavedMessageItems` — mock SSE émet : message item A → 2 text deltas → function call → message item B → 2 text deltas → completed. Asserte que les blocks A et B ont des indices différents et que les textes sont bien séparés.

### Effort estimé

100-150 LOC, 1 subagent, ~3 min.

---

## 3. Computer use tools (read_image + screenshot stub)

### Contexte

Le type `ImageSource` est en place (commit 52392bd). Les tools eux-mêmes ne le sont pas. Subagent précédent timeout sans produire les fichiers tools.

### Cible

- `internal/tools/computer_use.go` : `ReadImageTool()` + `ExecuteReadImage(ctx, input)` ; `ScreenshotTool()` + `ExecuteScreenshot(ctx, input)` (stub avec `*api.APIError{StatusCode: 501, Message: "not yet implemented for this platform"}`).
- `pkg/api/tools/computer_use.go` : re-exports publics.
- `internal/tools/computer_use_test.go` : tests décrits ci-dessous.

### Comportement read_image

Input : `{path: string}` ou `{url: string}` (un des deux).

- Si path : valider taille < 5 MB, lire le fichier, deviner le media type (`image/png`, `image/jpeg`, `image/gif`, `image/webp`), encoder base64.
- Si url : valider scheme == "https" (refuser http/file/data), fetch, valider Content-Type, lire le body, encoder base64.

Output : `{description: string, blocks: []api.ContentBlock}` où `blocks` contient un seul `ContentBlock{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "...", Data: "..."}}`.

### Tests requis

- `TestReadImage_Base64FromFile` — t.TempDir() + fichier PNG minimal, asserte le block construit.
- `TestReadImage_RejectsLargeFile` — 10 MB file → erreur mentionne taille.
- `TestReadImage_RejectsHTTPURL` — http://… → erreur mentionne https.
- `TestScreenshot_NotImplemented` — asserte `*api.APIError{StatusCode: 501}`.

### Effort estimé

200-300 LOC, 1 subagent, ~5-7 min.

---

## 4. MCP OAuth broker (PKCE flow)

### Contexte

`internal/mcp/oauth/storage.go` (218 LOC, commit 4a42881) est en place : token storage atomique. Manque le broker.

### Cible

- `internal/mcp/oauth/broker.go` :
  ```go
  type Broker struct{ ... }
  func NewBroker(opts ...Option) *Broker
  func WithRedirectPort(port int) Option
  func WithAuthOpener(fn func(string) error) Option
  
  type ServerConfig struct {
      Name, AuthURL, TokenURL, ClientID string
      Scopes []string
  }
  
  func (b *Broker) Acquire(ctx context.Context, cfg ServerConfig) (string, error)
  func (b *Broker) Revoke(ctx context.Context, serverName string) error
  ```
- `internal/mcp/oauth/broker_test.go` avec httptest + mock browser opener.
- Wiring dans `internal/mcp/auth.go` et `internal/mcp/registry.go` pour appeler `broker.Acquire(...)` quand un MCP server répond `auth_required`.

### Détails techniques

- PKCE : verifier 43-128 chars (RFC 7636), challenge = base64url(SHA-256(verifier)), `code_challenge_method=S256`.
- Local callback : `http.Server` sur `127.0.0.1:<port>`, handler `/oauth/callback` capture `code` + `state`.
- State : random 32-char URL-safe.
- Browser opening : `WithAuthOpener` injectable, défaut = print URL avec « ouvrir manuellement ».
- Refresh : si `expires_at - now < 30s` et `refresh_token` présent → POST `grant_type=refresh_token`.

### Tests

- `TestPKCE_GeneratesValidVerifierAndChallenge`
- `TestBroker_RefreshOnExpired` (httptest token endpoint)
- `TestBroker_AuthCodeFlow` (httptest auth+token endpoints, `WithAuthOpener` programme une requête vers le callback)
- `TestBroker_StateMismatch_Rejects`

### Effort estimé

400-500 LOC, 1 subagent, ~10-15 min — **trop pour un subagent unique**. Découper en 2 :
- Sous-feature A : PKCE + browser opener + Acquire flow happy-path (~250 LOC)
- Sous-feature B : refresh, revoke, state mismatch, mcp/auth.go wiring (~200 LOC)

---

## 5. Session timeline + lineage CLI

### Contexte

Session data persistée dans `internal/runtime/session_jsonl.go`. Pas de commande `claw session timeline` ni `claw session lineage`. Subagent timeout sans produire de code.

### Cible

- `internal/commands/session_timeline.go` :
  - `claw session timeline <id>` : rendu chronologique de events.jsonl avec timestamps relatifs, emoji par event type, one-line summaries.
  - `claw session lineage <id>` : ASCII tree du parent + enfants forks.
- `internal/commands/session_timeline_test.go` : 4 tests (ordering, truncation, lineage tree, no-forks single node).

### Format de sortie

Voir le doc précédent : `docs/claw_port_progress.md` (généré dans la session précédente, mais probablement gitignored sous `.live-runs/`). Le format ASCII est documenté dans le prompt subagent qui a timeouté.

### Effort estimé

250-350 LOC, 1 subagent, ~7 min.

---

## 6. OTLP telemetry exporter

### Contexte

Subagent précédent a échoué à produire du code, perdu en délibération entre hand-craft protobuf et import de `go.opentelemetry.io/proto/otlp`.

### Cible

- `internal/apikit/telemetry/otlp/exporter.go` :
  ```go
  type Exporter struct{ ... }
  func New(cfg Config) (*Exporter, error)
  func (e *Exporter) Export(ctx, events) error
  func (e *Exporter) Start(ctx) error  // batch flusher goroutine
  func (e *Exporter) Stop(ctx) error
  ```
- Sink interface dans `internal/apikit/telemetry.go` :
  ```go
  type Sink interface{ Emit(event TelemetryEvent) }
  func RegisterSink(s Sink)
  ```
- Auto-init si `OTEL_EXPORTER_OTLP_ENDPOINT` set.

### Décision technique

**Utiliser** `go.opentelemetry.io/proto/otlp` (acceptable per la spec de la session précédente — pas le SDK complet, juste les protos). Hand-crafting le wire format protobuf est une distraction.

Format : OTLP/HTTP avec body protobuf. Mapper chaque `TelemetryEvent` en `LogRecord`.

### Tests

- `TestExporter_BatchesAndFlushesOnSize` (batch_size threshold)
- `TestExporter_BatchesAndFlushesOnInterval` (timer)
- `TestExporter_RetriesOn5xx` (httptest 503→200)
- `TestExporter_StopFlushesPending`
- `TestSink_FanOutToMultiple`

### Effort estimé

300-400 LOC, 1 subagent, ~10 min.

---

## 7. Plugin marketplace + CLAUDE.md slash command auto-load

### Contexte

Subagent timeout sans produire de code. La feature couvre 2 deliverables distincts.

### Deliverable A : marketplace HTTP

- `internal/plugins/marketplace.go` — fetch catalog JSON, search/filter
- `internal/plugins/installer.go` — download tarball, verify SHA-256, extract
- `internal/plugins/manager.go` — install/uninstall/list state on disk
- Wire `claw plugin install/uninstall/list/search` slash commands

### Deliverable B : CLAUDE.md auto-load

- `internal/commands/claudemd_loader.go` — scan ancestor CLAUDE.md files, parse `## /commands` blocks, register dynamically.

### Effort estimé

500+ LOC total, **trop pour un subagent**. Découper :
- A.1 : marketplace HTTP client + tests (~200 LOC)
- A.2 : installer (download+checksum+extract) + tests (~150 LOC)
- A.3 : manager + slash command wiring (~150 LOC)
- B : CLAUDE.md loader + tests (~150 LOC)

---

## Ordre d'attaque recommandé

Par ordre de ratio impact/effort, et en évitant les conflits de fichiers entre tâches parallélisables :

1. **BUG 4 ctx propagation** — petit, complète l'intégrité du système hooks (déjà à 90%).
2. **BUGs 2/3 Responses interleave** — petit, ferme un latent bug.
3. **Computer use tools** — moyen, débloque un cas d'usage majeur (vision agents).
4. **Session timeline UI** — moyen, données déjà là.
5. **OTLP exporter** — moyen, observabilité production.
6. **MCP OAuth broker (Sous-feature A puis B)** — gros, débloque MCP distant.
7. **Plugin marketplace (4 sous-features séquentielles)** — le plus gros, peut attendre.

**Conseil** : dispatcher 1, 2, 3 en parallèle (fichiers distincts). Puis 4 + 5 en parallèle. Puis 6.A. Puis 6.B + 7.A en parallèle. Puis 7.B → 7.C → 7.D séquentiellement.

---

## Pointeurs

- État commits : `git -C /workspaces/iterion/.works/claw-code-go log origin/master..HEAD --oneline`
- Tests existants : `cd .works/claw-code-go && devbox run -c /workspaces/iterion -- go -C $(pwd) test -short -race ./...`
- Doc parité : `.works/claw-code-go/docs/parity.md`
- Doc progression : `iterion/docs/claw_port_progress.md`
- Memory : `~/.claude/projects/-workspaces-iterion/memory/project_claw_parity_port_2026-04-28.md`

Bon courage à l'agent suivant. Cette ligne de travail mérite d'être achevée — claw-code-go est à un cheveu de la parité COMPLETE avec Claude Code.
