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


## The price feed (ADR-163)

The operator table has an auto-synced twin: org/team feeds in the
database (`price_feeds`), refreshed by one elected replica on each
feed's interval, folded at every launch into the run bundle and planted
on the run's environment — a fresh entry answers before the operator
table for the ids it carries, a stale one steps down to it, and the
`_gateway_spec` record carries `"feed:<name>"` with its age. Manual
sync and the feed list: `iterion remote price-feeds`. The full contract
— precedence, sources, drift — is
[backends.md's price-feed section](../../backends.md#the-gateway-price-feed-auto-synced-operator-pricing-adr-163).

Accepted efforts use `OPENAI_COMPATIBLE_REASONING_EFFORTS`, separate from the
strict pricing table. Invalid entries warn and are ignored individually;
values normalize case/whitespace and accept integers 1–100. Old runners ignore
the variable, so no deployment order is required. Delegation, interaction and
cut-turn recovery share the mapping. Only closed-list, typed provider parameter
refusals park as `PROVIDER_REQUEST_INVALID`; model prose and tool stderr do not.
A sandboxed node's refusal arrives typed only from a runner image of the same
release: an older runner sends its text alone, which stays `EXECUTION_FAILED`.
The [gateway and correction contracts](../../backends.md#the-openai-compatible-gateway-openai_compatible)
list the values, deployment example and Claude Code's untyped-error limitation.
