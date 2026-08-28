# 🧠 The model registry, and choosing which model runs

Two questions this answers, both of which used to have no answer at all:

1. **Which models can I actually use for a run from here?** — `iterion models`,
   `GET /api/models`, and the studio's model pickers, all off one code path.
2. **Which model does the studio assistant run on, and how do I change it?**
   — the model picker on the session launcher / header, persisted per user.

Design record: [ADR-090](adr/090-model-registry-and-operator-model-choice.md).

---

## The registry

A model entry crosses four sources that already existed separately:

| source | answers |
|---|---|
| `model.KnownModelSpecs` | which specs the catalog ENUMERATES (curated list) |
| the models.dev aggregator | fresher capabilities/prices for those specs |
| [`pkg/backend/detect`](../pkg/backend/detect/detect.go) | local: what THIS host holds credentials for |
| tenant launch tiers (BYOK, OAuth-forfait, platform) | cloud: what a *run for this tenant* will receive |
| `llmtypes.ModelCapabilities` | context window, tool-calling, reasoning, temperature |
| [`pkg/backend/cost`](../pkg/backend/cost/cost.go) `EffectiveRate` | what a run is actually charged |

**The enumerated set is the curated one.** The aggregator enriches specs the
catalog already names; it does not widen the list. `detect` probes more
providers than `KnownModelSpecs` covers (xai, foundry, bedrock, vertex have no
curated rows), so on a host holding only one of those the registry answers
"nothing usable" while the CLI would happily run the provider's models. Ask
about one explicitly — `?spec=xai/grok-4` on the endpoint, `iterion models
xai/grok-4`, or the picker's **Custom…** entry — and it resolves normally.
Widening `KnownModelSpecs` (or enumerating the aggregator) is the fix; until
then this is the registry's known blind spot, not a claim that the model is
unreachable.

[`pkg/modelcatalog`](../pkg/modelcatalog/catalog.go) is the crossing, and is
the single code path behind both the CLI and the HTTP endpoint — the two
cannot disagree about whether a model is reachable. In cloud the HTTP
endpoint no longer feeds it the control-plane `detect.Report`: that is the
host the *server* process sees, not the bundle `cloudpublisher` seals for
the runner.

### From the CLI

```sh
iterion models                       # the curated set, with reachability + price
iterion models anthropic/claude-opus-5
iterion models --refresh             # re-fetch the models.dev spec cache first
iterion models --json                # the full modelcatalog.Catalog envelope
```

The `USABLE` column lists the backends that can drive the model **right now**;
`no` means none can, and the reasons are printed underneath:

```
Not reachable from this host:
  openai/gpt-5.5 — no credential detected for provider "openai"
  anthropic/glm-5.2 — no credential detected for provider "zai"
```

### Over HTTP

```
GET /api/models
GET /api/models?spec=openai/gpt-5.5&spec=anthropic/claude-opus-5
GET /api/models?refresh=1
```

`spec` is repeatable (and accepts a comma-separated list) — that is how the
launch form asks about a bot whose nodes pin models outside the curated set.
`refresh=1` re-probes host credentials **and** re-fetches the aggregator.

Only capability values and credential **source names** (`ANTHROPIC_API_KEY`)
cross the wire — never a credential value.

Each row carries `reachability`:

| value | meaning | picker |
|---|---|---|
| `local` | proven from the host process (CLI / local studio) | blocking if `usable=false` |
| `cloud` | proven from the authenticated tenant's launch tiers (BYOK, user/org OAuth-forfait, platform) | listed as available for this team's runs |
| `unknown` | cloud, but no launch-tier proof — a pool grant or runner-env fallback *may* still serve | warning, **not** a blocking "unreachable" |

Cloud **never** treats a control-plane env key as tenant reachability, and
**never** treats a tenant BYOK/OAuth that the server process lacks as
unreachable. The pool is not probed (proving it would acquire a grant), so
those models stay `unknown` rather than a false yes or a false no.

The catalog envelope also stamps `reachability: "local"|"cloud"` for the
surface that was evaluated.

### Two mappings that are easy to get wrong

- **The spec prefix is the API dialect, not the vendor.** The GLM family
  speaks the Anthropic API, so its specs read `anthropic/glm-*` — but the
  credential is z.ai's (`ZAI_API_KEY`, or `ANTHROPIC_BASE_URL` pointed at
  z.ai). An `ANTHROPIC_API_KEY` does **not** unlock GLM; it 401s.
- **`claude_code` carries its own OAuth credential.** A signed-in Claude Code
  CLI makes Claude models usable with no `ANTHROPIC_API_KEY` at all. It cannot
  be pointed at another vendor, so it never makes an OpenAI or GLM model
  usable.

### What "usable" does and does not promise

The catalog reports reachability from **credential presence**. It never calls
the provider, so a revoked or over-quota key still reads as usable until a run
fails. Models that are *not* reachable are listed rather than hidden — seeing
that a model exists but wants a credential is what tells you which key to set.

A zero price means **no published price**, never free. The `price_known` flag
(`—` in the CLI, in the picker) is what distinguishes the two.

---

## Choosing the model for a run

Any LLM node's model, backend and reasoning effort can be retargeted at launch
without editing the `.bot`. Full reference:
[docs/backends.md § Launch-time per-node/-group overrides](backends.md#launch-time-per-node-group-overrides).

```sh
iterion run bots/whole-improve-loop/main.bot \
  --model 'reviewer_*=anthropic/claude-fable-5' \
  --backend '*=claw' \
  --effort-for 'fix_*=max'
```

In the studio, the Launch form's **Model & backend per node** section is a
picker over the registry: options are grouped reachable / needs-a-credential,
each carrying its context window, price and tool-calling gap, with a
`Custom…` escape hatch for any `provider/model-id` the registry omits.

### The capability guard

The choice is not neutral, so the picker says so **before** launch rather than
letting it be discovered mid-run:

| condition | level | why |
|---|---|---|
| no credential can reach the model (`reachability` local or cloud) | blocking | the run fails at its first node |
| cloud reachability is `unknown` | warning | launch-tier proof is missing; a pool grant or runner fallback may still serve |
| the model has no tool-calling | blocking | the agent loses the board, skills and run introspection — broken, not degraded |
| `ultracode` on anything but `claude-opus-4-8` | warning | it degrades silently to plain `xhigh` (diagnostic C089, [docs/ultracode.md](ultracode.md)) |

Nothing is disabled: the guard informs, the operator decides. The host's
recommended model stays one click away so a cheap pick is undoable.

---

## The assistant's model

The studio assistant (Nexie, `bots/whats-next`) used to run on whatever
`ITERION_WHATS_NEXT_MODEL_CLAUDE` / `ITERION_WHATS_NEXT_EFFORT_CLAUDE` said at
**server start** — not per user, not per session, invisible in the UI.

It now carries a model picker in two places:

- the **session launcher**, so the model is chosen before the first message;
- the **session header**, next to the spend. A change there applies to the
  **next** session — a live run keeps the model it started on.

The choice is remembered per user (and per team in cloud) and re-applied to
every later session, so it survives the session rather than being re-made.

**A run keeps its model for its whole life, not just its first node.** This is
worth stating because it is the part that is easy to get wrong and impossible
to notice: a conversational run pauses on its chat node, so **every operator
reply is a resume** — and a resume carries no launch spec. The overrides are
read back off the run document (`run.model_overrides`) on all three resume
paths: in-process ([`Service.resumeExecutorSpec`](../pkg/runview/service_launch.go)),
the CLI (`iterion resume`, which the detached studio path shells out to), and
cloud (`SubmitResume` republishes them on the queue for the runner pod).

On the CLI, `--model` / `--backend` / `--effort-for` given to `resume`
layer **on top** of what the run was launched with, per field — so
`--effort-for '*=high'` re-targets the effort without discarding the model.

The picker also sends the **backend that can drive the chosen model**. The
assistant surface deliberately has no backend control (you are choosing a
model, not an execution stack), but the bot pins `backend: "claude_code"`, so
selecting an OpenAI spec without a matching backend produced a session that
died at its first node with an error about the backend rather than about the
choice.

```
GET    /api/v1/preferences/model?key=<scope>
PUT    /api/v1/preferences/model      {"key":"…","model":"…","backend":"…","effort":"…"}
DELETE /api/v1/preferences/model?key=<scope>
```

Two things about the API worth knowing:

- `set` in the response distinguishes **never recorded** (fall back to the
  bot's own defaults) from **recorded and deliberately empty**.
- An **omitted dimension clears** the stored one. That is how you return to
  the bot's default for just the effort while keeping your model.

`key` is **opaque** — the studio passes a bot id and the engine never
interprets it. That is deliberate: iterion the engine must not know that one
particular catalog bot is "the assistant" ([CLAUDE.md](../CLAUDE.md) — the
engine stays bot-agnostic), and it means a second conversational bot needs a
bundle, not an engine change. Storage still bounds this caller-controlled
namespace: a key is at most **128 bytes**, uses letters/digits plus
`._:/-`, and each `(tenant, user)` may record at most **64 distinct keys**.
Existing keys remain updateable at the limit; a new one returns HTTP `409`.

### Where it is stored

| mode | store |
|---|---|
| local / desktop | `<store-dir>/model-prefs.json` ([`modelprefs.FileStore`](../pkg/modelprefs/filestore.go)) |
| cloud | the `model_prefs` Mongo collection, keyed `(tenant, user, key)` |

A server with neither wired 404s the endpoints; the studio then still offers
the picker and says the choice applies to that browser tab only. A corrupt
prefs file degrades to "no preference" (with a warning in the server log).
Before the next write repairs it, the original bytes are atomically preserved
as `model-prefs.json.corrupt.bak` with mode `0600`; if that backup fails, the
repair is aborted and the corrupt original is left untouched.

### Cloud: the choice travels with the run

A remembered preference only matters if the machine that executes the run
honours it. In cloud mode the launch is published to the runner pool, so the
model/backend/effort selection rides `queue.RunMessage.ModelOverrides` and the
pod folds it back through the same conversion the in-process path uses — what
the run Overview shows and what the pod calls are the same values.

That field arrived with **queue schema v=6**, and a runner rejects a message
whose version it does not recognise. **Upgrade the server and the runner pods
together**: a runner left on v=5 fails every launch with
`queue: schema version 6 unsupported`. That is deliberate — the alternative was
a pod quietly running the bot's default model while the studio displayed the
one you picked. If you see that error, the fleet is version-skewed, not
misconfigured.

Launch-time overrides **are** replayed on resume for runs launched through the
studio / HTTP API, in both modes: the rows stamped on the run document are
re-folded by `runview.Service.Resume` in-process and republished by
`cloudpublisher.SubmitResume` in cloud. `iterion resume` merges them beneath any
`--model` / `--backend` / `--effort-for` typed for that attempt — the
flag wins per field, so re-targeting only the effort keeps the launch's model.

This holds on every launch surface, `iterion run` included: the CLI stamps the
rows it parsed onto the run document, so a run launched from the terminal
inherits its own choice on resume and shows it in the studio Overview like any
other.

---

## Troubleshooting

**"No models are usable"** — nothing on the host holds a credential. Check
`iterion models` for the per-model reason, then `GET /api/backends/detect` (or
the studio toolbar's backend pill) for the provider-level picture. On a laptop
the usual fix is signing into the Claude Code CLI, which unlocks the Claude
models with no API key.

**"I set `ANTHROPIC_API_KEY` but GLM is still unusable"** — expected. GLM
needs a z.ai credential; see the dialect-vs-vendor note above.

**"The assistant feels dumber"** — check the model in the session header. A
small model, or `ultracode` on a model that is not `claude-opus-4-8`, both
degrade reasoning quality with no other signal. "Back to bot default" in the
picker restores the bot's own pins.

**"My preference is not sticking"** — the picker says so when the server
cannot persist it (no prefs store wired). Otherwise check the store paths
above.
