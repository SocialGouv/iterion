# ADR-056 — Adaptive transient retry + bounded auto-resume

Status: **accepted** (2026-07-03; shipped `f84ad6af6`).

## Context

Dogfooding `whole_improve_loop` (run `019f2247`) required **11 manual
`iterion resume` invocations** over ~9h. 9 of the 11 failures were transient
backend errors (`backend "claude_code" failed` = rate-limit / session-limit /
idle-watchdog / network); 2 were `BUDGET_EXCEEDED` (duration) resolved by
raising `--max-duration` and resuming. Every one of these is mechanically
recoverable, yet each stopped the run at `failed_resumable` waiting for a human
to type the same resume command. That toil is the recovery gap this ADR closes.

The existing idle watchdog (returns a retryable "session idle for" error) and
`rateLimitSignals` classification (incl. "hit your session limit", ADR-052-era)
already caught *some* transient errors for in-executor retry, but the
classification was incomplete and the run-level retry was entirely manual.

## Decision

Two layers of **bounded, fail-loud** automatic recovery. Both preserve the
"errors are explicit, never a silent fallback" rule: a non-transient / logic
failure still fails loudly, and every retry path is bounded.

1. **In-executor transient retry (Layer 1).** Tightened the transient-error
   classifier (`isDelegateRetryable`) with verbatim CLI/HTTP2 connectivity
   markers the slow run-level classifier caught but the fast in-executor loop
   missed, and made the retry budget explicit
   (`ITERION_NODE_MAX_TRANSIENT_RETRIES`). Retryable: typed
   `ErrTransient`/`ErrRateLimited`, network signatures, idle-watchdog, `signal:`
   kills / exit ≥128. Terminal (no retry): exit 1/2/127, schema
   "missing required field" (uses the validate-retry path), auth 401/403, plain
   logic errors. Retry-After is honored (claw already does, `40a7148f1`).

2. **Bounded run-level auto-resume (Layer 2).** Opt-in `--auto-resume N`
   (+ `ITERION_AUTO_RESUME`, default 0 = off) on `iterion run` and `resume`.
   On a `failed_resumable` exit whose RuntimeError code is in a retryable
   allow-list (`EXECUTION_FAILED` transient, `BUDGET_EXCEEDED`, `TIMEOUT`,
   `RATE_LIMITED`, `NETWORK_TRANSIENT`, `TOOL_FAILED_TRANSIENT`), the CLI
   re-invokes resume in-process — reusing the same launch overrides — with
   capped exponential backoff, up to N times, emitting a `run_auto_resumed`
   event each time. Fails **closed**: unclassified / `SCHEMA_VALIDATION` /
   `AUTH_FAILED` / `WORKSPACE_SAFETY` / `LOOP_EXHAUSTED` / FailNode /
   user-cancelled / human-paused never auto-resume.
   - **Budget special-case:** `BUDGET_EXCEEDED` auto-resumes only if a higher
     `--max-*` cap is in effect; otherwise it stops with a clear message rather
     than re-trip the same cap in a loop.
   - **Forfait-cap awareness (best-effort):** before an auto-resume that would
     draw on the Claude Code OAuth forfait, `pkg/backend/forfait` checks
     Anthropic usage (`GET /api/oauth/usage`, `anthropic-beta:
     oauth-2025-04-20`, Bearer from `~/.claude/.credentials.json` — used only
     as a header, never logged). If 5h or 7d utilization ≥ `ITERION_FORFAIT_CAP_PCT`
     (default 85), it does **not** resume: it stops in `failed_resumable` with
     `forfait cap 85% reached (5h=../7d=..%), resume later`. If the endpoint is
     unreachable / no token / an API key is set (metered, not forfait), the
     check returns `Skipped` and the loop proceeds by count only — the cap
     never blocks on uncertainty.

## Consequences

- The 11-manual-resume toil is replaced by `--auto-resume N`, forfait-safe
  (won't burn quota past 85%) and budget-safe (won't loop on an unraised cap).
- Default off — no behavior change unless opted in; every non-transient failure
  still surfaces.
- `run_auto_resumed` makes automatic recovery observable in the timeline
  (distinct from a manual resume).
- Complements the per-unit incremental-commit redesign
  ([ADR-055](055-unit-convergent-adaptive-improve-loop.md)): auto-resume keeps a
  long run alive, incremental commit ensures each recovered stretch lands work.
