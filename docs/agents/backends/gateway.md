# The OpenAI-compatible gateway

Serving `openai_compatible/<gateway model id>` from the deployment's
environment: the two variables, the verbatim wire id, the guarded dial and
its escape hatch, the claw-only routing and its C186 warning, the catalog
(ADR-122) and what an unknown answer costs, the env-funded accounting.

Read [backends.md](../../backends.md#the-openai-compatible-gateway-openai_compatible)
— the section is the reference; this page is the pointer.

## Provider/model configuration lives in the DB

The database is the seat of provider/model configuration — context window,
prices, capabilities, the per-credential default model (spec and tracking:
#2906). Every provider/model record resolves through one cascade,
global → org → team → bot → bot-in-team → bot binding, which extends the
credential audience chain (`OrderedAPIKeyCandidates`, `pkg/secrets/byok.go`)
and the model-override fold (`model.ModelOverrides`). The env tables
(`OPENAI_COMPATIBLE_MODELS`, `OPENAI_COMPATIBLE_CATALOG_PROVIDER`) are
runner-side fallbacks, not the seat of truth.


The operator catalog can declare `reasoning_efforts` per route. Unsupported
requests choose the nearest accepted level with a `reasoning_effort_adjusted`
event and log; invalid provider requests park as `PROVIDER_REQUEST_INVALID`.
The [catalog and correction contracts](../../backends.md#the-openai-compatible-gateway-openai_compatible)
describe the declaration and the runtime's advisory/strict output boundary.
