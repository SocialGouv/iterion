# Trusted metering

`delegate.Result` carries harness accounting independently of model JSON:
`Tokens`, `CostUSD`, `UnreportedCalls`, and the session-total cost flag.
Backends populate these fields from provider usage; retries and fallback chains
fold the fields, never counters read from `Output`. The sandbox wire carries
the same fields. Custom backends and runners must implement this contract;
putting a counter only in `Output` no longer reports spend.

`stampDelegateOutputMeta` owns the reserved `_` output namespace. It removes
unknown keys and stale metadata, then projects the typed result, including zero
tokens. Success, failure, schema retry, structured recovery, router and sandbox
relay paths use this boundary. Recovery replaces the answer map but retains the
primary result's bill. Engine degradation/session metadata attaches afterward.
The documented `_needs_interaction` / `_interaction_questions` request protocol
is the deliberate model-authored exception; these keys never report spend.

An interaction auto-answer returns answers and a separate spend map. Both
runtime interaction modes book that map on success, failure and escalation,
before re-invocation or checkpointing. Answers retain only the reserved keys
that were explicitly asked; accounting never overwrites such an answer.

Witnesses live in `pkg/backend/model/trusted_metering_test.go` and
`pkg/runtime/trusted_metering_test.go`. Their comments name the mutations:
restore nil-only tokens, remove the namespace sweep, restore failure-only
interaction booking, or remove any boundary call. The first two run fixtures
book the harness's 42 tokens despite model JSON claiming 0 or 99999999; the
interaction fixtures book 100 tokens without forwarding accounting as answers.
