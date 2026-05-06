# ADR-003: Pure-Go privacy tools (`privacy_filter` / `privacy_unfilter`)

- **Status**: Accepted
- **Date**: 2026-05-06
- **Authors**: devthejo
- **Workflow context**: `examples/privacy_pipeline.iter`,
  `docs/privacy_filter.md`

## Context

Iterion needed a built-in mechanism to detect and redact PII (emails,
phones, URLs, IBAN/CC numbers, secrets) inside tool nodes so that:

1. LLM agents can be denied raw PII while preserving downstream
   restoration (sanitize → process → unsanitize), and
2. Persisted run state (`events.jsonl`) is scrubbed by default for
   the privacy tools, so a long-lived `.iterion/` directory does not
   become a PII archive.

The first iteration of this work prototyped a Python sidecar driving
the [openai/privacy-filter](https://huggingface.co/openai/privacy-filter)
ONNX model. That prototype turned a 30-second cold start, a hard
dependency on a `task privacy:install` step, and a non-trivial
distribution problem (every binary release would need a matched
Python toolchain or a compiled-in ONNX runtime) into the user's
problem.

The categories iterion actually needed coverage for fall in two
camps:

- **Structural / regex-detectable** — `email`, `phone`, `url`,
  `account_number`, `secret`. These are the territory of
  industrial-grade Go tools like
  [gitleaks](https://github.com/gitleaks/gitleaks)
  and [truffleHog](https://github.com/trufflesecurity/trufflehog),
  both of which ship as a single binary with hundreds of curated
  regex patterns.
- **Contextual** — `person`, `address`, `date`. These genuinely need
  a model to disambiguate "Avenue Foch" (an address) from "Avenue
  Foch" (a club name in Paris), or "Sunday" (a day) from "Sunday"
  (a person's name).

The 5 structural categories are the operational priority — the
flagship use case (anti-secret commit gate) is purely a regex
problem. The 3 contextual categories can ship later via an opt-in
backend.

## Decision

Implement the v1 privacy tools entirely in Go using regex +
heuristics (Shannon entropy, Luhn, mod-97 IBAN). Ship the 5
structural categories. Defer person/address/date to a v2 ML
backend that hosts can opt into.

The detector lives at `pkg/backend/tool/privacy/detector/` as a
standalone subpackage with no iterion-specific imports. The
registration layer (`pkg/backend/tool/privacy/`) wires the detector
into the iterion tool registry and persists vault entries to
`<storeDir>/runs/<runID>/pii_vault.json` (`0600`).

The `RegisterClawAll` defaults gain a `Privacy *privacy.Config`
field; runview wires it in `BuildExecutor` so every iterion
launch has the tools available, gated only by the workflow's
`tool_policy`.

## Trade-offs

| Dimension | Pure-Go (chosen) | ONNX sidecar (rejected) |
|---|---|---|
| Distribution | One static binary, zero setup | Python + pip + venv + ONNX runtime per platform |
| Cold start | < 1 ms | 10-30 s (model load) |
| Per-call latency | Microseconds (regex on 100 KB < 50 ms) | Single-digit ms once warm, but I/O + serialize overhead |
| Reproducibility | Bytes-identical detector across machines | Pinned model hash + matching ONNX runtime |
| Attack surface | Pure stdlib (`regexp`, `crypto/sha256`, `math`, `os`, `sync`) | + Python interpreter + transitive deps + IPC channel |
| Coverage | 5 categories | 5 + person/address/date |
| Adversarial robustness | RE2 → no backtracking, no DoS | Model-dependent; adversarial prompts can mislead it |
| Calibration | Industry-standard rule sets (gitleaks-derived) | Black-box; HF model accuracy varies on token formats |
| Tunability | Per-rule scoring, custom postFilter, easy to add patterns | Retraining required for new categories |
| Failure mode | Predictable (false negatives if pattern absent) | Opaque (model hallucination on novel inputs) |

The single concession is the deferred categories. We accept that:
1. The deferred categories are contextual and rarely the bottleneck
   for the workflow patterns iterion targets (orchestration,
   refinement, anti-secret commit gates).
2. A v2 backend can be added behind the same `Detector` interface
   without changing the public tool surface; existing redact-then-
   restore workflows would Just Work with richer detection.
3. The v1 surface is honest: callers who genuinely need person-name
   redaction are told so up-front (`docs/privacy_filter.md`),
   instead of getting a model that produces 60% recall and silent
   leaks the rest of the time.

## Alternatives considered

### 1. ONNX sidecar driving `openai/privacy-filter`

The original direction. Combined a curated HF model with a
sub-process speaking JSON over stdin/stdout to the iterion binary.

**Rejected because**: the operational footprint dominated the
benefit. Reproducibility depended on a HuggingFace cache, a Python
version, a torch wheel that ships per-platform, and a model file
that periodically gets re-uploaded with breaking changes. Cold
start meant the first redact in a workflow blocked for 10-30 s,
which is incompatible with the multi-step refine loops iterion is
designed for. Coverage on `secret` patterns was *worse* than a
gitleaks-derived ruleset because the model is calibrated against
prose, not opaque token formats.

### 2. Cross-compile gitleaks/truffleHog into iterion

Both tools are MIT and ship as Go binaries. We could `go get` them
and call out via subprocess.

**Rejected because**: subprocess invocation per redact (typical
workflow: hundreds of agent calls, each preceded by a redact)
imposes process-startup overhead that microsecond regex calls
don't have. Also, vendoring a CLI binary inside iterion feels
clunky. Going pure-Go internally and **deriving** rules from
gitleaks (with attribution) gives us their pattern catalogue
without the runtime indirection.

### 3. Opaque hash placeholders (no vault)

Make redaction one-way: emit `[PII_<8hex>]` and have no
restoration tool.

**Rejected because**: workflow #4 in `docs/privacy_filter.md`
(redact → LLM-draft-reply → restore) is the killer feature for
regulated contexts where the LLM provider is not in scope to
process identifiable data. Without the vault, the placeholder text
flows out of the workflow and the human reviewer sees it, not the
restored draft. Removing the unfilter capability would gut the
flagship use case.

### 4. Generic persistence-aware redaction mechanism

Provide a way for any tool to declare "my input field X / output
field Y must not enter events.jsonl", driven by tool metadata.

**Rejected for v1**: no other tool needs this in v1. The hard-coded
`switch toolName` in `executeToolNode` and the mirror in
`buildNodeFinishedData`'s `sanitizeOutputForEvent` is two short
helpers totalling ~60 LOC. Generalising would require a registry
hook contract, a way to express the redaction rule, and tests for
the generalised path — disproportionate ahead of the second
caller. We will revisit if a third privacy-sensitive tool surfaces.

## Deviations from the source plan

The plan called for a default placeholder template of
`[PII_{token}]` (with `{token}` substituting to the bare 8-hex
suffix or the full `PII_xxxxxxxx` atom — the plan was internally
inconsistent). Implementing it surfaced a round-trip issue:

- Token = `PII_a3f5b1c2`, template = `[PII_{token}]` produces
  `[PII_PII_a3f5b1c2]`; the unfilter regex `PII_[0-9a-f]{8}`
  matches the second `PII_` and substitutes back, leaving
  `[PII_<original>]` in the output — corrupted.
- Token = bare hex, template = `[PII_{token}]` produces
  `[PII_a3f5b1c2]`; the unfilter regex must include the brackets
  to round-trip cleanly, which couples the regex to the default
  template and breaks every custom template.

We chose the simplest deterministic alternative: **default
template is `{token}`** (no decoration), token is `PII_<8hex>` (the
plan's preferred atom shape), unfilter regex is `PII_[0-9a-f]{8}`.
The redacted text reads `Hello PII_a3f5b1c2 !` instead of `Hello
[PII_a3f5b1c2] !`. Custom templates that include the literal token
still round-trip; templates that embed `{token}` inside delimiters
also round-trip but leave the user's delimiters in the unfiltered
output (documented as the user's choice).

## Consequences

- **One self-contained binary.** `iterion run` works immediately
  after `iterion init`; no `task privacy:install`, no environment
  variables, no model downloads. Operators with offline machines
  see no behavioural difference.

- **Hard-coded persistence-aware redaction.** Two specific tool
  names (`privacy_filter`, `privacy_unfilter`) are recognized in
  `pkg/backend/model/executor.go` and `pkg/runtime/helpers.go`. A
  third privacy-sensitive tool would need a parallel entry here
  and motivate generalisation.

- **Extensibility.** The `detector.Detector` type is a single
  interface boundary. A v2 ONNX backend could implement the same
  `Scan(text, opts) []Span` contract; callers' wiring code would
  not change. The `Detector` field on `privacy.Config` already
  accepts any `*detector.Detector` — host applications could
  swap in a richer detector at startup once the v2 backend lands.

- **Tier-2 categories explicitly out of scope.** The output schema's
  `categories` enum lists only the 5 v1 categories. Workflows
  needing person/address/date redaction must either pre-process
  inputs themselves or wait for the v2 backend. This is documented
  prominently in `docs/privacy_filter.md`.

- **Distribution win.** No new entries in `go.mod`. No CGO. The
  `iterion` binary's footprint grows by ~250 KB (the regex
  catalogue + helpers).
