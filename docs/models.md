# 🧠 The model registry, and choosing which model runs

Two questions this answers, both of which used to have no answer at all:

1. **Which models can I actually use on this host?** — `iterion models`,
   `GET /api/models`, and the studio's model pickers, all off one code path.
2. **Which model does the studio assistant run on, and how do I change it?**
   — the model picker on the session launcher / header, persisted per user.

Design record: [ADR-087](adr/087-model-registry-and-operator-model-choice.md).

---

## The registry

A model entry crosses four sources that already existed separately:

| source | answers |
|---|---|
| `model.KnownModelSpecs` + the models.dev aggregator | what iterion knows about |
| [`pkg/backend/detect`](../pkg/backend/detect/detect.go) | what THIS host holds credentials for |
| `llmtypes.ModelCapabilities` | context window, tool-calling, reasoning, temperature |
| [`pkg/backend/cost`](../pkg/backend/cost/cost.go) `EffectiveRate` | what a run is actually charged |

[`pkg/modelcatalog`](../pkg/modelcatalog/catalog.go) is the crossing, and is
the single code path behind both the CLI and the HTTP endpoint — the two
cannot disagree about whether a model is reachable.

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
| no credential can reach the model | blocking | the run fails at its first node |
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
bundle, not an engine change.

### Where it is stored

| mode | store |
|---|---|
| local / desktop | `<store-dir>/model-prefs.json` ([`modelprefs.FileStore`](../pkg/modelprefs/filestore.go)) |
| cloud | the `model_prefs` Mongo collection, keyed `(tenant, user, key)` |

A server with neither wired 404s the endpoints; the studio then still offers
the picker and says the choice applies to that browser tab only. A corrupt
prefs file degrades to "no preference" (with a warning in the server log) and
is repaired by the next write — the worst honest outcome is re-picking a model.

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

Launch-time overrides are **not** replayed on `iterion resume` in either mode;
a resumed run returns to the bot's DSL defaults unless the flags are passed
again.

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
