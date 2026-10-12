# ADR-122 — A gateway's models are resolved exactly: the operator's table, then a qualified catalog key, offline included

- Status: accepted (2026-10-01, #2028 slice 4)
- Date: 2026-09-30
- Deciders: jo (direction), Claude (analysis)
- Note: numbered 122 — 118 is the merge-gate ADR on main, 119/120/121 are claimed (#2062, the agent-tree slice, and #2119's adaptive-routing ADR, merged 2026-10-02)
- Relates to:
  - [042-dynamic-model-specs-curated-fallback-async-fetch.md](042-dynamic-model-specs-curated-fallback-async-fetch.md) —
    amended for gateway ids only, in two places: its third rejected alternative (an embedded table), and the
    consequence "`ITERION_MODEL_SPECS=off` forces pure curated".
  - [093-model-spec-registry-as-a-leaf-package.md](093-model-spec-registry-as-a-leaf-package.md) —
    amended for gateway ids only: §3 (claw's price tier first) and the consequence "`off` also disables the
    cost tier". Its §5 rule is followed as-is: unknown capability flags are omitted, never invented.
  - Tickets: #2028, #2029, #2005, #2038, #2103 (capability flags), #2099 (operations boundaries).

## Context

A deployment, or a dedicated runner pool (#2029), can serve `openai_compatible/<gateway id>` from its
environment. The gateway is any OpenAI chat-completions endpoint, such as LiteLLM in front of a regional
host, or vLLM. The gateway id is whatever the gateway serves.

We measured a regional LiteLLM gateway. It serves **bare** ids (`gpt-oss-120b`, `deepseek-v4-flash-0731`, …),
and its model-info endpoint is forbidden to the key. The same models exist in models.dev under the upstream
host's provider key (`scaleway/<id>`).

Today's lookups may attach unrelated numbers to such an id, or return unknown:
- **claw's price tier** matches the id's last path segment against OpenRouter's prices;
- **`modelspecs.Lookup`** falls back to a consensus-filtered bare index;
- **the static table** prices `gpt-4o` at OpenAI's rate.

A gateway alias is not a vendor's model. The gateway decides what an alias serves.

An unknown context window compacts at claw's unknown-model threshold (10 000 estimated tokens). That severely
degrades long tool loops. An unknown price makes a node look free to budgets.

The deployments that need a gateway most sit behind a boundary that blocks models.dev. Sandboxed nodes price
and compact inside their container, which has neither the cache nor a route to models.dev.

## Decision

1. **Resolution order for `openai_compatible/<id>`.** Sources are tried one whole entry at a time; fields are
   never merged across sources.
   1. **The operator's table.** `OPENAI_COMPATIBLE_MODELS` maps an exact id to `context_window`,
      `max_output_tokens`, `input_usd_per_mtok` and `output_usd_per_mtok`, as JSON. Decoding is strict:
      unknown fields are refused, and prices come as both or neither.
   2. **Otherwise, the catalog.** When `OPENAI_COMPATIBLE_CATALOG_PROVIDER=q` is set, the lookup uses the
      exact key `q/<id>` in the models.dev table. It takes the live or cached table when that table is loaded
      and holds the key, else the embedded snapshot. "Exact" means exact after the parser's lowercasing of
      model keys.
   3. **Otherwise, unknown.** An explicit warning names both knobs. The answer is never a bare, suffix, claw
      or static-table one.
2. **One immutable record per invocation.** The host resolves once per dispatched invocation, fallback
   element or subbot. The record is `{spec, source, key, as_of, window, max_output, prices}`, and an explicit
   unknown is a record too.
   - Every consumer reads that record: compaction, output annotation (`_gateway_spec`), and the sandbox
     forward (`ITERION_OPENAI_COMPATIBLE_RESOLVED`, the host's answer taking precedence container-side so the
     image's older snapshot cannot disagree across the IPC).
   - Shipped in this slice: compaction sizing, `_gateway_spec`, `Result.ContextWindow`, the estimator branch,
     the sandbox forward. Capability FLAGS stay deferred (#2103 — the struct needs an unknown state before a
     gateway id can declare them), as does the cost-preview incompleteness marker.
   - Accepted residual: the live models.dev table refreshes on a background TTL, so a long tool loop CAN
     cross a refresh — the compaction threshold of a later turn reads the newer table while the stamped
     record is the invocation's final reading. The estimator reads the same static-plus-live sources the
     record does, so the stamp and the price can never disagree; only the window can move mid-loop, which is
     the same semantics every vendor model already has.
   - The record travels to a sandboxed node beside the gateway requirements it must support.
   - The pricing tables are never read again in mid-flight, so a table refresh cannot price one node twice.
3. **What the record drives:**
   - **the price.** The gateway branch runs first in cost estimation, ahead of claw's tier. A price is known
     when both rates are positive.
   - **the compaction threshold**, as `window × the effective ratio` (the authored ratio when set, else 0.85).
     An unknown window keeps claw's unknown-model threshold.
   - **the reported max output and the capabilities response.** Flags the catalog does not carry are omitted.
4. **The snapshot.**
   - **Content:** the flattened models.dev table with its four numeric fields, as canonical JSON with one
     entry per line and sorted keys, plus a header (schema version, source digest, as-of date).
   - **Location:** committed at `pkg/backend/modelspecs/snapshot/models-dev.json` and embedded in the binary
     (about 0.9 MB).
   - **Access:** only through the gateway's exact lookup; it is never indexed into the general registry.
   - **Refresh:** `task models:snapshot`. The committed diff is the review.
   - **Scope of `ITERION_MODEL_SPECS=off`:** it does not disable the snapshot, since reading an embedded file
     is not a fetch.
   - **Age:** a snapshot answer older than 180 days logs a warning.
5. **Provenance travels with the answer.** It appears on the node output, in the step events, in the logs,
   and in the capabilities and cost-preview responses. A preview that contains an unknown route is marked
   incomplete; it never reports a partial total as the complete one.
6. **The gateway is env-funded.** Its spend is attributed to the environment, never to a bundle credential,
   and is not reported as a dropped route. A route whose model or provider depends on `${…}` counts as
   unresolved, never as env-funded, because the publisher's environment is not the runner's.

## Alternatives rejected

- **Operator table only.** Every model must be declared by hand, and a correct public catalog entry goes unused.
- **Qualifier with the live table only.** It fails exactly where a gateway is needed: behind a boundary that
  blocks models.dev.
- **The snapshot as a floor for every lookup.** It brings back ADR-042's drift for vendor models.
- **A bare, consensus, claw or static-table lookup.** Each can price an alias as a different publisher's model.
- **The gateway's own model-info endpoint.** It is gateway-specific, and the measured key cannot read it.
- **A gzipped snapshot.** It is smaller in the binary, but it turns every refresh into an opaque blob that a
  reviewer cannot read. Git already stores the readable file as deltas.
- **Per-consumer re-resolution.** Each meter re-reads the tables at its own moment, so one invocation can be
  priced from two sources.

## Consequences

- A declared or catalogued gateway model gets its price and window offline, and an unknown one is loud.
- The binary grows by about 0.9 MB.
- The snapshot drifts between refreshes. Every answer it gives carries its as-of date.
- ADR-042's reasoning still holds for every non-gateway lookup, because the snapshot is not a general fallback.
- A declared price of 0/0 still reads as unpriced. Declaring a model free is a follow-up.
