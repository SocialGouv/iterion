# ADR-090 — Runtime-configurable operational settings, DB-backed with TTL-bounded propagation

- **Status**: Accepted
- **Date**: 2026-08-24
- **Extends**: the platform LLM-credential doctrine (docs/cloud-llm-credentials.md), ADR-073 (no local-only durable seams)
- **First settings family**: the usage-cap percentages (`pkg/usagecap`)

## Context

The subscription usage caps were frozen at process start from env
(`ITERION_USAGE_CAP_5H_PCT` / `ITERION_USAGE_CAP_WEEK_PCT`): both the
server's launch-time pre-flight and the runner's claim-time/mid-run guard
called `usagecap.FromEnv()` once and carried the result for the life of
the process. Changing a cap in production meant `kubectl set env` on TWO
deployments plus a rolling restart, and the two enforcement points could
diverge silently — one deployment rolled, the other not, each enforcing a
different number with no surface saying so.

The platform already solved this exact class for provider credentials:
DB-backed records, mutable through the authenticated super-admin API,
effective without restart, with the env value as the fallback tier.

## Decision

Apply the same doctrine to operational settings:

1. **One platform-scoped record** (`platform_settings` collection, doc
   `_id: "usage_caps"`; `usagecap.SettingsStore`, Mongo + memory twins).
   A nil field inherits the env default — a deployment that never touches
   the API keeps exactly its env-configured behaviour. A future settings
   family is a new document, not a schema change.
2. **A TTL-cached resolver** (`usagecap.Resolver`, 30s TTL) lays the
   record over the env-resolved policy. Every enforcement point reads
   through the `usagecap.PolicySource` seam **per evaluation** — the
   runner's pre-flight, the mid-run `Guard` (which now consults its
   source on every `Observe`), the server's launch pre-flight, and the
   `/healthz` echo. Propagation bound = the TTL; the mutating pod
   invalidates its own cache in the handler.
3. **Percentages only.** The soft/hard modes and the
   `ITERION_USAGE_CAP` kill switch stay env-only: they encode the
   deployment's enforcement *posture*. The kill switch wins — a zero env
   policy carries no modes, so a DB percentage laid over it stays inert
   and a runtime write can never re-arm a guard the operator explicitly
   disarmed ("never silently replace an operator's explicit choice").
4. **Merge-semantics PUT** on `/api/admin/settings/usage-caps`
   (super-admin guard, same as the platform LLM-credential routes):
   number sets, `null` clears, absent untouched; unknown fields and
   non-integers rejected 400 with the reason; every update audited with
   old value, new value and caller.

## Alternatives rejected

- **Push invalidation (bus/watch) instead of TTL polling.** A NATS
  fan-out or Mongo change stream would shrink the propagation bound to
  ~0, but adds a delivery path that can fail silently — exactly the
  divergence class this closes — and the caps tolerate 30s of staleness
  by nature (they fire on percent thresholds of multi-hour windows). The
  lossy-bus rule in CLAUDE.md would demand a reconciliation net anyway;
  the TTL poll IS the net, so ship only the net.
- **Making the modes runtime-mutable too.** Flipping `soft`→`hard` at
  runtime changes whether in-flight work is killed — a posture decision
  that should be witnessed by a deploy, not a hot toggle. Cheap to add
  later as new record fields if the need materialises.
- **A generic key-value settings table.** A typed record per family
  keeps validation (integer 0–100) and merge semantics in the owning
  package; an open KV store would push both to every reader.

## Consequences

- Retuning a cap is one `iterion remote admin caps set` call; both
  deployments converge within ≤30s; `/healthz` (`usage_cap` +
  `usage_cap_source`) is the no-DB verification surface.
- The runner ensures the `usage_windows` schema unconditionally (a cap
  can be armed at runtime, and must find its readings ledger), and a
  LIVE source keeps a per-run guard even while nothing is capped — the
  answer can change before the run ends. An env-only static disabled
  policy still skips the guard entirely.
- Settings reads fail toward the last-known value (env before the first
  success), retried once per TTL window — a settings-store outage never
  changes enforcement abruptly in either direction.
- The next operational setting that needs a runtime path should join
  this record family rather than grow another env-only knob.
