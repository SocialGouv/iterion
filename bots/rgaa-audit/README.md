# rgaa-audit (Acci ♿) — RGAA / WCAG accessibility audit on the ultra11y engine, read-only

Acci audits a project's UI source against the **RGAA 4.1.2** (or the WCAG
2.2 AA core) with the [ultra11y](https://github.com/SocialGouv/ultra11y)
engine and reports what the engine can stand behind: a dated French
conformance report, a per-page grid when pages were captured, a coverage
annex that names every layer that ran, a replayable verdict ledger and one
board issue per non-conforming criterion. It reports and files; it never
edits the audited code.

The split is **who finds, who judges, who refuses**:

- the **engine** finds — its static rules decide every criterion they can,
  anchored `file:line`, reproducible without a model — and refuses: its own
  fold rejects an unjustified verdict, an uncited conformity or an
  ungroundable non-conformity by name, and its integrity check rejects a
  report that cites what does not resolve;
- **read-only agents** judge the criteria a static pass cannot decide, one
  batch per theme, in parallel, from the evidence the engine harvested and
  the normative brief it writes per criterion at run time — no RGAA text
  lives in this bundle;
- a **second reader** puts every stated non-conformity and every landed
  conformity on trial; an optional **human gate** approves or declares
  criteria undecidable; a **transcription judge** with no tool but the board
  files the plan verbatim, and a deterministic node checks it did.

The engine is pinned by version **and** by the sha256 of its entry file
(`ultra11y.pin`), fingerprinted before anything of it runs and before every
call. Credits: [CREDITS.md](CREDITS.md).

## When to use it

A read-only conformance pass whose findings must be defensible — pre-release
review, recurring conformance tracking, a non-regression run after fixes.
For **fixing**, use Willy (`whole-improve-loop`) with the `rgaa` preset on
the issues Acci files.

## How it runs

```
prepare ─► static_audit ─► bank_static ─► render_gate ─► [render] ─► ledger_replay
  ─► worklist ─► plan_batches ─► dispatch ⇒ adjudicate ─► write_fragment
  ─► merge_verdicts ─► fold_probe ⟲ (re-ask of the named refusals, once)
  ─► fold_final ─► draft_report ─► budget_gate ─► [refute ⇒ … ─► refute_apply]
  ─► review_gate ─► [human_review] ─► final_report ─► deliver ─► [publish ─► publish_verify] ─► done
```

- `prepare` resolves the pinned engine (`$ULTRA11Y_BIN`, the global npm
  install, `$ULTRA11Y_HOME/<version>`, else `npm install` into the run
  directory), refuses anything that does not carry the pinned fingerprint
  (`ENGINE_HASH_MISMATCH` / `ENGINE_UNRESOLVED`), probes the browser and
  writes the run's helper library.
- `static_audit` is THE finding set; an empty scope is `EMPTY_SCOPE`, never
  a clean audit. `bank_static` publishes it before any model runs.
- `render` scans the page sample (`.ultra11yrc.json`) in a Playwright
  Chromium when `rendered=auto` finds one, serving the site with
  `serve_command` if given; `rendered=required` makes its absence a
  resumable `RENDER_UNAVAILABLE`.
- `ledger_replay` replays the committed ledger through the engine, which
  re-grounds every entry; the stale ones, and those its gate refuses (named),
  go back to the agents. Stored recommendations are not replayed: they are
  non-normative, and one whose element has since been edited would refuse
  the verdict it rides on. A ledger the engine cannot read is
  `LEDGER_INVALID`, with the engine's own message.
- `worklist` → `plan_batches` → `dispatch` (fan-out per batch) →
  `adjudicate` (read-only agent) → `write_fragment` (item-keyed) →
  `merge_verdicts` (one writer, later rounds win per criterion, citations
  checked against the shown evidence, recommendations kept only on the
  criterion's own anchors — the others dropped and counted) →
  `fold_probe` (the fold on a copy, refusals named and re-asked once) →
  `fold_final` (the real audit folded exactly once, ledger merged; a retyped
  citation folded with its anchor's text where the engine accepts both).
- `draft_report` renders the draft and the trial worklist; `budget_gate`
  skips the second reader, visibly, past `refuter_budget_share` of the cost
  cap; `refute_apply` applies a complete trial with `--prune` (the engine's
  own findings are never pruned) and reports what it pruned; an incomplete
  trial is not applied, and the part of an applied one the engine refuses
  (a retained claim whose anchor does not ground) is named as a degradation.
- `final_report` uses ultra11y 5.45.0 to render the business summary and
  its separate technical annex. It cross-checks the annex's « Décidés »
  line and the summary's RGAA rate against the audit, then runs the engine's
  integrity and completeness checks. Both draft readers receive both files.
  `report_dir`, when set, receives these documents, requested HTML files,
  the linked screenshots, Markdown and JSON page grids when rendered,
  Iterion's coverage annex
  and the ledger with workspace-relative anchors. Relative report links
  remain usable. `report_files` lists the source artifacts; historical
  outputs and issue plans are not swept into this export.
- `deliver` builds the issue plan from the engine's own tickets (one per
  criterion, severity-sorted, capped); `publish` files it when
  `post_to_board` is true; `publish_verify` holds the judge's own report to
  the plan and fails typed when a planned title is neither created nor found
  (a self-report: a tool node has no board action to read the board with).

Every node that rewrites `audit-latest.json` records its hash in the run's
`state/` directory and its own output; every later node refuses a document
that no longer matches (`AUDIT_INTEGRITY`), and a node re-entered on a
resume re-emits its recorded output instead of folding twice.

Every refusal the bot makes is a typed `fail` node (`ENGINE_UNRESOLVED`,
`ENGINE_HASH_MISMATCH`, `EMPTY_SCOPE`, `RENDER_UNAVAILABLE`,
`LEDGER_INVALID`, `AUDIT_INTEGRITY`, `ADJUDICATION_INCOMPLETE`,
`REFUTE_INCOMPLETE`, `HUMAN_REJECTED`, `REPORT_INTEGRITY_FAILED`,
`GRID_INCOMPLETE`, `BOARD_PUBLISH_INCOMPLETE`); an engine crash stays an
untyped, resumable error — except on a committed ledger the engine cannot
replay, which is `LEDGER_INVALID` with the engine's own message.

## Variables that matter

| var | default | meaning |
|---|---|---|
| `standard` | `rgaa` | the RGAA 4.1.2 pack (the WCAG core report has no « Décidés » line and a different rate semantics: a follow-up) |
| `scope_globs` / `exclude_globs` / `max_files` | whole workspace | what the engine audits |
| `rendered` | `auto` | `off` / `auto` / `required` — the browser layer; the engine writes its captures under `.ultra11y/pages/` of the workspace (gitignore it) |
| `serve_command` / `serve_ready_url` | — | how to serve the site for the scan |
| `ledger_path` | `.ultra11y/verdicts/<standard>.json` | the committed ledger to replay |
| `batch_size` / `max_reask` | 8 / 1 | adjudication batches and re-asks |
| `refute_enabled` / `refuter_budget_share` | true / 0.6 | the second reader and its budget guard |
| `human_review` | false | pause on a human gate before the final report |
| `require_complete` | false | degraded layers become typed failures |
| `post_to_board` / `board_state` / `board_epic_label` / `findings_cap` | false / inbox / — / 80 | the board transcription (the command, schedule and board invocations set `post_to_board=true`); `findings_cap` 0 files nothing |
| `report_dir` | — | where the deliverables are copied; empty = run artefacts only |

Models and efforts are env dials: `ITERION_RGAA_MODEL_ADJUDICATE`,
`ITERION_RGAA_MODEL_REFUTE`, `ITERION_RGAA_MODEL_PUBLISH`,
`ITERION_RGAA_BACKEND_ADJUDICATE`, `ITERION_RGAA_BACKEND_REFUTE`,
`ITERION_RGAA_EFFORT_*`, `ITERION_RGAA_MAX_DURATION`.

## Running it

```sh
iterion run bots/rgaa-audit --var scope_globs='src/**' --var rendered=off \
  --var report_dir="$PWD/audits" --max-cost-usd 30
```

A local run audits the worktree iterion creates from `HEAD`: commit
`.ultra11yrc.json` and the ledger before launching, or the run does not see
them. Budget about 2 $ per batch of at most 8 criteria with the default
models, and pass `--loop-budget-guard off` when you resume a run that hit
its cap, or the re-ask is declined.

`iterion validate --exec --strict --fixtures bots/testdata/rgaa-audit-dryrun-fixtures.json bots/rgaa-audit/main.bot`
walks the whole graph without a model, a shell or a workspace.

## Tests

- `task test:contract:rgaa` — the engine contract on the real pinned engine
  (`bots/rgaa_audit_engine_contract_test.go`, read
  [engine contracts](../../docs/agents/testing/engine-contracts.md)) and
  the bot's own tool nodes, run as the real scripts against a planted
  fixture site, agents replaced by fixture verdicts, with mutations.
- `bots/rgaa_audit_engine_pin_test.go` — the pin, the bot's defaults, the
  runner image's ARG and the lockfile held equal.
- `task test:live:bot:rgaa-audit` — a real run on the fixture site (real
  LLM cost).

## Skills

`rgaa-audit` (the playbook), `ultra11y-engine` (the engine from inside the
bot), `adjudication-protocol`, `refutation-protocol`, `iterion-board` (the
shared board skill).
