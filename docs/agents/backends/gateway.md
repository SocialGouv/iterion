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
and the model-override fold (`model.ModelOverrides`). A gateway model's
capabilities — window, output cap, prices, accepted reasoning efforts — are
facts of the gateway: their seat is the price feed below, at the platform,
org and team levels only, never a bot, binding or credential (ADR-166). The
env tables (`OPENAI_COMPATIBLE_MODELS`, `OPENAI_COMPATIBLE_REASONING_EFFORTS`,
`OPENAI_COMPATIBLE_CATALOG_PROVIDER`) are runner-side fallbacks, not the seat
of truth; a platform `scaleway` feed is `CATALOG_PROVIDER`'s database seat.

## The price feed (ADR-163, ADR-166)

The operator table has a database twin: platform, org and team feeds
(`price_feeds`, documents `platform`, `org:<id>`, `team:<id>`), folded at
every launch and resume into the run bundle and planted on the run's
environment. Team over org over platform, per id: within a level the
first answering feed that prices an id wins its price entry, which answers
WHOLE — prices, window, output cap, cache price and flags from that one
feed, never merged (ADR-122); a feed that prices a declared id restates
its window, output cap and cache price, or they are lost, and the fold
warns. Accepted efforts fold apart by the same precedence, so a priced
entry never erases a declared effort list. A fetched feed (`modelsdev`, `scaleway`,
`litellm`, `exec`) refreshes on one elected replica and answers while fresh;
a stale one steps down to the operator table. A `declared` feed is the
operator's table stored in its config (`params.entries`): never fetched,
never stale. Its entries answer whole as the operator-table rows they
copy — priced or not — and an entry stating only `reasoning_efforts`
answers its efforts alone. The `_gateway_spec` record carries
`"feed:<name>"` with its age.

Commands: `iterion remote price-feeds list|set|delete|sync|declare
[--platform|--org|--team]` (the platform level is super-admin) and
`effective --team T [--model ID …]`, the table a run launched now for the
team (naming those ids) seals, each answer with its feed and level. The full contract —
precedence, sources, declarations, drift — is
[backends.md's price-feed section](../../backends.md#the-gateway-price-feed-auto-synced-operator-pricing-adr-163).

## Accepted efforts

A gateway model accepts only some `reasoning_effort` values; the runner maps
the requested effort onto that list (nearest named level, ties toward more
effort). `compatgw.AcceptedEfforts` reads the run table's `efforts` map first
(`feed:<name>`), then `OPENAI_COMPATIBLE_REASONING_EFFORTS` by exact wire id
(`env:…`), else the effort passes. Efforts fold per field, separately from
the price entries: a feed pricing an id never erases a declared list. The
env table stays tolerant (invalid entries warn and drop individually; values
normalize case/whitespace and accept integers 1–100), and old runners ignore
the variable, so no deployment order is required. Delegation, interaction
and cut-turn recovery share the mapping (`TestEveryReasoningEffortWriterIsInventoried`);
the sandbox crossing writes the host's merged table under the env name, so
an older sandbox image honours the database. `reasoning_effort_adjusted`
names its `source`.

Only closed-list, typed provider parameter refusals park as
`PROVIDER_REQUEST_INVALID`; model prose and tool stderr do not. A sandboxed
node's refusal arrives typed only from a runner image of the same release:
an older runner sends its text alone, which stays `EXECUTION_FAILED`. The
[gateway and correction contracts](../../backends.md#the-openai-compatible-gateway-openai_compatible)
list the values, deployment example and Claude Code's untyped-error
limitation.
