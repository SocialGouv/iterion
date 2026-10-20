# Run the RGAA audit yourself — the demo path

`rgaa-audit` (Acci) audits a product against the RGAA 4.1.2 standard and
produces a dated French report: conformance rate, findings per criterion,
a page grid, a coverage annex and the verdict ledger. The engine
([ultra11y](https://github.com/SocialGouv/ultra11y), pinned and
hash-verified) owns the judgement; iterion orchestrates the LLM
adjudication around it.

The fastest full tour audits **this repository's own studio** — the
dogfood target the sample and the committed ledger were built for.

```sh
task demo:rgaa-audit
```

## Prerequisites

- The repo toolchain: `devbox install` (node ≥ 22.18, go, pnpm — the
  Taskfile drives everything else).
- An authenticated `claude` CLI: the run's agents delegate through the
  `claude_code` backend, so they spend YOUR Claude budget. A full first
  run is ~$20-30 (`RGAA_DEMO_BUDGET` overrides the ceiling); the run
  replays the committed ledger (`.ultra11y/verdicts/rgaa.json`), so
  criteria that did not change are not re-adjudicated.
- A free port 4899 (the task fails fast when something already listens
  there — a leftover studio e2e serve, for instance).
- ~10-20 minutes of wall clock, most of it in the LLM rounds.

## What the task does

1. Builds the studio SPA and the iterion binary with the bot catalogue
   embedded.
2. Resolves the pinned ultra11y engine (`scripts/ultra11y-engine.sh` —
   it prefers a matching global npm install; otherwise the first use
   installs the engine under `~/.cache/iterion/ultra11y` after
   verifying the pin's size and sha256).
3. Installs the chromium build the engine's rendered scan navigates with.
4. Runs `iterion run bots/rgaa-audit` with `rendered=required`: the
   studio is served locally (`studio/e2e/serve.mjs`, seeded workspace),
   the 21-page sample (`.ultra11yrc.json`) is captured with Chromium,
   and every layer — static audit, rendered capture, ledger replay,
   LLM adjudication, counter-reading — runs to the final report.

The report, page grid, coverage annex and ledger land under
`.rgaa-demo/<UTC-timestamp>/`; the run's own state lives in
`.rgaa-demo/store`, `home` and `run`. The rendered captures are the
exception: the engine writes them under the audited checkout's
`.ultra11y/pages/` (gitignored, like the e2e workspace `serve.mjs`
seeds under `studio/e2e/.tmp/`) — so don't run the demo while a studio
e2e session is live. The audit reads the checkout AS IT IS: uncommitted
edits to `studio/src` are part of what gets judged.

## Options and variants

- `RGAA_DEMO_BUDGET=50 task demo:rgaa-audit` — raise the spend ceiling.
- Post findings to the run's native board instead of stopping at the
  report: add `--var post_to_board=true` to the `iterion run` line (the
  board lives in the `--store-dir` store, `.rgaa-demo/store` — the
  GitHub 203 bridge is a separate, operator-owned push).
- Audit a DIFFERENT repo: copy the task's `iterion run` line, point
  `scope_globs` at the target, set `rendered=off` if it has no page
  server, and write your own `scope_notes`. The committed ledger only
  covers this studio; another repo starts from an empty ledger —
  expect ~$60 for ~100 criteria, so raise the ceiling accordingly
  (`RGAA_DEMO_BUDGET=80` or `--max-cost-usd 80`).
- `iterion resume --run-id <id> --store-dir .rgaa-demo/store …` resumes
  an interrupted run (add `--loop-budget-guard off` if the guard trips
  on the resumed budget).

## Limits

- The task is local-only by design: it pins `rendered=required`, and a
  missing browser under `required` FAILS the run (typed, not degraded).
  On a machine without chromium, drop to the static layer explicitly
  with `--var rendered=off` — the report then says so in its annex.
- `--sandbox none` ships in the task because the rendered layer needs a
  local browser and a local server; the bot's own nodes are read-only
  (no shell, no writes), so the exposure is the page capture, not your
  tree.
- The sample covers the studio's seeded pages; cloud-only screens
  (login, admin, teams) are out of sample.
- The weekly CI advisory (`ci/a11y-advisory`) runs the deterministic
  layers only (no LLM); a red advisory names the criteria the next run
  will re-adjudicate.
