# instrument (Obsy) — run bilans

Newest first. Bot: [bots/instrument/](../../bots/instrument/) — observability
instrumentation campaign (Sentry/GlitchTip error tracking + standardized
logs; opt-in tracing).

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
