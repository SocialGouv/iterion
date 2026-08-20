# instrument (Obsy) — run bilans

Newest first. Bot: [bots/instrument/](../../bots/instrument/) — observability
instrumentation campaign (Sentry/GlitchTip error tracking + standardized
logs; opt-in tracing).

## 2026-08-20 — second dogfood: the opt-in tracing family (run 01a01db3)

- Status: **validated**
- Versions: bot 0.1.0 (+ the #460/#462 contract fixes) · iterion `fb4a453d5` (v3.50.2)
- Method: same chassis, `scope=errors,logs,tracing` (the tracing opt-in
  exercised for the first time), `--max-cost-usd 45 --merge-into none`,
  mission_notes = tracing arbitrations (dial via SENTRY_TRACES_SAMPLE_RATE
  read in errtrack.Init — sentry-go does not read it natively; sentryhttp
  on the API server behind an identity-when-off errtrack.HTTPMiddleware;
  exactly ONE hand seam: the in-process LLM call; no run-length
  transactions).
- Result: **converged in ONE pass** (vs 3 on the first dogfood — the
  clean-tree clause and the verify-build GOFLAGS gotcha landed between
  the two runs and no friction recurred). $29.10, 41.6 min, 6 commits on
  `iterion/run/blastoff-wail-pyrebloom-b28b` (`c5a2424`).
- Value: `pkg/errtrack` tracing dial (off unless the env parses > 0, loud
  on unparsable, Config.TracesSampleRate test seam), one Sentry
  transaction per API request named by route, one span per provider call
  in pkg/backend/model/generation.go, tracing_test.go + server
  tracing_test.go on the in-memory transport, docs/observability.md
  Tracing section + ADR-088 dated addendum. Host verification: full
  `task check` exit=0 (128 pkgs). Live smoke: ephemeral server with
  rate=1 → the two `http.server` transactions visible in the project's
  Performance page, named by route.
- Findings / misses: the campaign verified errors+logs already complete
  and spent the whole pass on tracing — the "expect zero work there"
  mission note prevented re-litigating. It also self-documented the
  one-surface scope choice (final commit). Nothing over-claimed; review
  clean on the first pass.
- Engine/bot hardening: none needed — the run surfaced no new engine or
  bot friction (first frictionless run of the fleet's chassis on this
  repo).
- Lessons for next run: the fleet-propagated lessons demonstrably
  compound (3 passes → 1); a scope var carrying an already-done family
  costs one cheap verification, not a wasted pass.

## 2026-08-19 — first dogfood: instrument iterion itself (run 01a01a51)

- Status: **validated**
- Versions: bot 0.1.0 (bundle commit `719f1af`) · iterion `ed7f6ecad` (v3.48.3)
- Method: campaign/verify_build/review on claude_code · claude-opus-5
  (effort high / medium / high), sandbox auto (repo devcontainer, devbox
  toolchain), `--max-cost-usd 60 --merge-into none --var open_mr=false`,
  `scope=errors,logs` (defaults), `mission_notes` = pre-arbitrated iterion
  design (extend pkg/log; new pkg/errtrack on sentry-go; standard
  SENTRY_* envs; seams: CLI main / gosafe / alert.Sink / server+runner+
  dispatch; dispatch → JSON default; docs + ADR).
- Result: **converged pass 3** (cap 6+1), $51.44, 95.4 min, 282k tokens.
  18 instrumentation commits on `iterion/run/turbo-jolt-cryomantle-9c42`
  (final `4e997cc`; PR tip `0929e8ee` — the last commit is a wip-banked
  junk artifact, see friction 3). Host verification: full `task check`
  green (vet, gofmt, golangci-lint, 128 test pkgs, goldens, studio
  lint+tsc+vitest, pi-ext) at the PR tip.
- Value: complete errors+logs instrumentation of iterion —
  - `pkg/errtrack` (sentry-go v0.48.0 vendored): opt-in iff `SENTRY_DSN`,
    loud non-fatal init, release `iterion@<version>+<commit>` from
    appinfo, `SENTRY_ENVIRONMENT`, bounded `Flush(2s)`, key+value
    secret/PII scrubbing (`BeforeSend`/`BeforeBreadcrumb`), test-only
    `Transport` seam;
  - `pkg/log` warn+ hook seam → error=event (fields as context),
    warn=breadcrumb; "log lines never crash the producer" preserved
    (panic-safe hook dispatch);
  - capture seams: CLI top level (panic + fatal exit), `pkg/server`
    gosafe + detached goroutines, cloudpublisher detached tasks, runner
    background loops, `pkg/alert` errtrack sink (run_failed/stall/budget);
  - logs: `iterion dispatch` now defaults to JSON like server/runner;
    studio honours the log env vars; last stdlib `log` stragglers swept
    onto the seam; a run's own log lines stay in process format;
  - docs/observability.md runbook + ADR-088 + CLAUDE.md runbook-index
    pointer + the Conventions line rewritten (no more "no structured
    logging library");
  - unit tests with an in-memory transport (no network, DSN-unset no-op
    proof, scrubbing proof).
  Live smoke (operator, post-run): fake local ingest ← `iterion dispatch
  /nonexistent-config.yaml` with DSN ⇒ exactly one envelope
  (`level:error`, `environment:smoke-test`, `release:iterion@…`); DSN
  unset ⇒ byte-identical output + exit code, zero network.
- Findings / misses: pass 1 under-reported honestly (1 slice + a precise
  10-item plan — the contract's honesty clause working). Pass 3's fresh
  re-read found seams the mission notes had NOT listed (runner bg loops,
  server detached goroutines, run-log format interaction) — the
  "fresh pass re-reads everything" design paying off. Nothing
  over-claimed; review (in-loop, opus high) stayed clean and blocked
  nothing.
- Engine/bot hardening (frictions → fixes):
  1. **verify.sh authored without `-buildvcs=false`** — go's VCS probe
     fails (exit 128) in a sandboxed git worktree — and `verify_probe`
     reuses any syntactically-valid script forever, so the gate could
     never go green while the campaign kept (correctly) reporting
     complete. Operator patched the script mid-run; bundle fix committed
     (`22a9196`): the campaign contract now names the script path and
     licenses fixing the SCRIPT when the failure is the gate's, and the
     bundle's verify-build skill documents the GOFLAGS gotcha.
     **Follow-up (fleet):** the other 10 `verify-build.md` copies
     (4 variants) share the gap.
  2. **Pass boundary on a dirty tree** — pass 1 ended mid-slice with an
     un-vendored `go get` in the working tree; the deterministic gate
     verifies the TREE, so the half-slice red it as noise (one wasted
     verify cycle). Contract clause added (`22a9196`): end every pass on
     a clean tree (finish or revert). **Follow-up (fleet):** feature-dev
     and siblings share the exposure.
  3. **finalizeWorktree wip-banked 8 junk files** —
     `{e2e,pkg/*}/.claude/skills/deploy-target.md` mirrors (a skill from
     another bundle, mirrored into sub-agents' working dirs; nested
     `.claude/` is not gitignored, only the root one). Same junk sits
     untracked in the operator's main workspace, so it predates this
     run. The PR excludes the wip commit. **Follow-up (engine):**
     gitignore `**/.claude/` (or stop mirroring into non-root cwds).
- Lessons for next run: pre-arbitrated `mission_notes` are the quality
  lever — the campaign executed a 10-point repo-specific design with
  zero teach-back needed; `$60 / 3 passes` is the right budget shape for
  a repo this size; the docs' `iterion dispatch /nonexistent-config.yaml`
  smoke is the correct first live probe (a user-input error like a
  missing `.bot` is deliberately never reported).
- Adversarial review (post-run, 3 parallel reviewers, executed proofs):
  - *errtrack*: 2 high + 1 medium, all real — scrubFields was one level
    deep (nested `password:` reached the transport, and a CYCLIC logged
    value stack-overflowed the process via `%v`), event meta surfaces
    (ServerName/Environment/Release/…) unscrubbed, common token shapes
    (Slack/AIza/JWT/key=value) unmatched; plus bare `"auth"` in
    sensitiveKeys over-redacted `pr_author`. All fixed with red-first
    regression tests (commit on the instrumentation branch).
  - *bundle*: 1 medium — the frictions-fix clause licensing the campaign
    to EDIT verify.sh was a gate bypass (out-of-tree, invisible to the
    in-loop review, probe-reused forever, vacuous on targets without a
    CI drift gate; both halves executed). Reworded: the campaign may
    only DELETE the script, verify_build re-authors it. + instrument
    added to the verify_probe/verify_run guard-test lists; stale
    byte-share header fixed in the bundle's forge-mr-create copy.
  - *seams*: nothing high/critical — DSN-unset byte-parity, re-panic
    semantics, 2s flush bound (measured 2.01s on a hanging ingest),
    dispatch JSON default + human override all proven. Assumed as-is:
    the documented Fatalf→degrade on the infallible embedded-SPA path,
    the throwaway logger in claudesdk's defensive errorf, the discarded
    per-surface ServerName on second Init (cosmetic).
