[← Bot runs](README.md)

# docs-refresh (Doki) — bilans

Documentation refresh bot. Detects mismatches between project docs
(README, docs/**/*.md, CLAUDE.md, bundled skills, Go comments) and the
actual code, fixes the DOCS only (never code logic), and auto-commits on
convergence. Alternating claude_code (opus-4-8) / claw (gpt-5.5)
reviewers, deterministic `streak_check` (two cross-family approvals), a
`scan_docs` footprint enumerator + `build_manifest` anchor verifier so
agents can't truncate the audit set. Runs on ANY repo; iterion is the
reference self-host case.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: The deterministic audit machinery (scan_docs, scan_code_surface, build_manifest with the bounded severity-sorted/doc-chunked working set, author_docs DEFAULT-CREATE, mark_issue, audit cache) is kept in full; the alternating review/fix relay + its accumulators are replaced by ONE campaign that adjudicates the manifest and commits each aligned doc in stride. New deterministic gates: scope_check (writeable-set vs run base) + verify + coverage-in-gate; the cache is now fed by the manifest's mechanical verified_pairs. 16 nodes → 11 exec; worktree: auto.
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-22 — docs/studio-visuals branch self-review (run 019eef81)

- **Status: validated** — caught two real doc/code drifts, fixed both,
  and respected the steering (left the just-added screenshots, Mermaid,
  and cross-links untouched).
- **Versions:** bot manifest v0.14.0 · iterion binary v0.14.0
  (`36f19723f`), branch `docs/studio-visuals`.
- **Method:** dogfood right after a large docs round (new
  `human-in-the-loop.md`, a studio screenshot gallery, ASCII→Mermaid
  conversions, README hero). Ran **in place** on the worktree (no
  `worktree: auto`), host mode (claude_code/opus-4-8 + claw/gpt-5.5).
  Scoped to the **19 docs the branch changed** via `doc_globs`, plus
  `bundle_self_path=bots/docs-refresh`,
  `code_scope_globs=pkg/**/*.go,cmd/**/*.go`, `diff_since=main`, and
  `scope_notes` pinning "do not strip the intentional screenshots /
  mermaid / links". Store = worktree `.iterion`.
- **Result: converged in one review pass (~14 min), committed
  `727e384c0`** ("docs(dispatcher): correct per-ticket Bot routing and
  webhook test references", 2 files, +11/−7). `mark_issue_for_review`
  skipped (standalone run, no `issue_id`) → no board writes;
  `.docs-refresh-cache.json` gitignored.
- **Value: two genuine drift fixes.** (1) `dispatcher.md` documented
  the per-ticket `Bot` field as resolving into
  `DispatchSpec.WorkflowPath` — a struct field that **no longer
  exists** — and dismissed it as non-functional "future plumbing".
  Doki verified against `loop.go`/`runner.go` and rewrote it to the
  real behaviour (`Bot` → routing key `DispatchSpec.Assignee` →
  `RoutingRunner` selects the per-bot `EngineRunner`). (2) `byok.md`:
  stale test reference `TestGitLabWebhook` → `TestGitLabWebhook_HappyPath`
  (confirmed at `pkg/server/webhooks_gitlab_test.go:47`) + linked the file.
- **Findings / misses:** **zero over-reach** — it did not touch the
  freshly-added `images/studio/*.png`, ```mermaid blocks, or
  cross-links, and didn't churn the rest of the branch's prose.
- **Engine hardening:** none — clean run.
- **Lessons for next run:** scoping `doc_globs` to the branch's changed
  docs + `diff_since=main` + `code_scope_globs` gave a fast, focused,
  accurate pass (one cycle vs the 3 of the full-tree 2026-06-14 run).
  The `Bot`→`WorkflowPath` catch is textbook docs-refresh value:
  finding docs that name removed/renamed symbols.

## 2026-06-14 — repo-wide .bot→.bot CLI-example drift (run 019ec7ba)

- **Status: validated** — fixed exactly the drift the ticket flagged; correct,
  complete, scoped, intentional mentions preserved.
- **Versions:** bot v0.15.0 · iterion @ `8a00e93b` (main)
- **Method:** board ticket `a3b57a17` ("docs-refresh missed repo-wide .bot→.bot
  CLI-example drift"). docs-refresh has **no `worktree: auto` and no sandbox** →
  ran on the **live tree** (host mode: `claude` 2.1.177 on PATH, forfait forced),
  scoped to the 5 drifted files via `doc_globs`, `bundle_self_path=bots/docs-refresh`,
  `store-dir=.iterion`. Committed directly to main.
- **Result: converged + committed `211e69f7`** ("docs(cli): update CLI examples
  to use the .bot extension", **5 files, 23/23**). 3 reviewer cycles, **$8.40 /
  127k tokens**. Independently verified mid-loop: **0 runnable `.bot` left** in
  all 5 files, **zero over-reach** (nothing outside the 5 in-scope docs),
  `examples.md`/README/CLAUDE intentional "rejected/legacy" mentions untouched.
  `.docs-refresh-cache.json` is gitignored (no repo pollution). Board a3b57a17 → done.
- **Value: correct + scoped.** The commit message even states "Prose references
  to .bot as the DSL raw/source form are intentionally left unchanged" — the bot
  understood the preserve-intentional-mentions instruction.

### Findings / misses
- **Doki's automated scanners do NOT auto-detect CLI-example extension-literal
  drift.** `iterion run X.bot` in a code fence is not a dead link/anchor, so
  `md_link`/`build_manifest` don't flag it. Doki fixed it **only because
  scope_notes pointed the reviewers at it** — an *unguided* run would miss
  a3b57a17 again. The "miss" is a real scanner-coverage gap. Improvement idea:
  a CLI-example scanner that cross-refs example arg extensions against the
  CLI's accepted extensions (.bot/.botz).
- **cwd foot-gun (caught live by the Monitor).** For a bot with NO `worktree: auto`,
  the claude_code agents' cwd = the launch *process* cwd, **not** `--var
  workspace_dir`. First attempt isolated Doki in a worktree but launched from the
  main repo → reviewers got `File does not exist` (reading the wrong tree). Fix:
  to isolate a non-worktree:auto bot, launch **from** the target dir (cwd ==
  workspace_dir), or just run on the live tree. (workspace_dir only affects
  tool-node `git -C {{workspace_dir}}` commands, not agent cwd.)
- **Launch lesson:** a backgrounded `iterion run … | head -N` gets SIGPIPE-killed
  after N lines (head closes the pipe). Never pipe a long-running background
  command to `head` — redirect only.
- **Cost note:** $8.40 for a 23-line mechanical fix. Doki's opus review loop is
  expensive; for pure mechanical drift a manual edit is ~free. Reserve Doki for
  genuine doc-vs-code mismatches that need judgment, not trivial find-replace.
- Engine health clean: no `StructuredOutput` error; reviewer_gpt (claw/forfait)
  fine; the `fix_claude` Read-before-Edit + grep-exit-2 errors were transient
  self-corrections, not failures.

## 2026-06-14 — first dogfood + md_link scanner improvement (runs 019ec675, 019ec69f)

First recorded dogfood, on the real board ticket `c4043495` ("Align the
.bot documentation boundary"). Run in an isolated git worktree
(`--merge-into none`), store pointed at the operator's `.iterion` so the
run was visible in studio. Bot launched via standalone `iterion run` (not
the watchexec studio backend) and the install was a fresh static binary at
HEAD — both per the CLAUDE.md dogfood discipline.

- Status: **validated (with one real coverage finding, since fixed)**.
- Versions: bot 0.13.1 → **0.14.0** (this session) · iterion `e9148046`.
- Method: claude_code `claude-opus-4-8` + claw `openai/gpt-5.5`; isolated
  worktree; `--var doc_globs=CLAUDE.md,README.md,docs/**/*.md,pkg/cli/templates/*.bot,*.bot`
  `--var scope_notes="resolve .bot tension"` `--var bundle_self_path=bots/docs-refresh`.
- Result: **converged in 4 review iterations**, `$7.68`, ~126k tokens,
  ~27 min. Commit `e9520f11` on `dogfood/docs-refresh-boundary`.
  `.md`-only contract held; `prepare_commit` re-verified every code ref
  before committing (anti-façade discipline working).

### Value produced
- Caught + fixed **real drift**: `docs/secrets-reference.md` linked a dead
  path `pkg/auth/auth.go:GenerateRandomToken` — the function actually lives
  at `pkg/auth/password.go:118` (auth.go does not exist). Fixed, verified.
- `docs/bot-runs/whats-next.md` — clarified a local run-artifact path that
  read as a committed repo path.

### Finding (bot coverage gap) → FIXED this session
The bot **converged without resolving the ticket's headline item**:
`CLAUDE.md:3` still claimed "`.bot` / `.bot` — identical semantics" and
linked a **dead anchor** `README.md#iter-vs-bot` (the README heading was
removed; the CLI now rejects `.bot` outright — `unsupported workflow
extension`). The reviewers verify doc→**code** refs (symbols, CLI surface,
file paths under known roots) but nothing systematically audited
doc→**doc** internal links / `#heading-anchors`. `FILE_RE` in
`build_manifest` only matches paths under known roots (so bare `README.md`
slipped through) and never captured the `#anchor` fragment. The
`dead_link` taxonomy existed but had no deterministic candidate feeder.

**Fix (v0.14.0, `build_manifest`):** added an `md_link` anchor kind that
extracts `[text](path#anchor)` links and verifies BOTH the target file's
existence AND, for `.md` targets, the `#heading-anchor` (GitHub-slug
match: lowercase, strip non-`[\w\s-]`, spaces→hyphens, strip leading/
trailing hyphens to handle emoji headings; line anchors `#Lnn` skipped).
Drifted `md_link`s flow through the existing candidate pipeline at high
priority; `doc-mismatch-taxonomy.md` now points `md_link` → `dead_link`
(`anchor_kind: external`). Validated standalone over the full 153-doc tree
(**764 verified / 16 drifted, 0 false positives** after the slug fix), and
in a real scoped re-run (019ec69f) `build_manifest` flagged exactly the two
dead anchors (`CLAUDE.md:3` + `docs/examples.md:12` → `README.md#iter-vs-bot`,
`drifted_anchors: 2` of 288, zero FP). The scanner is generic — dead
internal links/anchors are a universal doc-drift class, not iterion-specific.

### Engine hardening
- Ticket **`d8e8dde1`** — **FIXED this session** (`3b29efb1`). Every
  claude_code node with schema + tools emitted `tool_error: No such tool
  available: StructuredOutput`: the agent (behaving natively, as the
  adaptivity work intends) reached for the SDK's `StructuredOutput` tool —
  available only under `--json-schema`, which iterion set in Pass-2, never
  Pass-1 — wasting its Pass-1 final turn (`raw_output_len: 0`) before the
  **unconditional** Pass-2 formatting round-trip. Root insight (verified
  empirically against claude 2.1.177): `--json-schema` composes with
  `--allowedTools` in ONE pass — the agent does its tool work, then calls the
  native StructuredOutput tool, populating `result.structured_output`. So the
  fix sets `WithOutputFormat` in Pass-1 even with tools, returns Pass-1's
  structured output directly when valid, and keeps the two-pass `formatOutput`
  as a fallback (max-turns / sandbox edge cases). Validated on run `019ec727`:
  both `reviewer_claude` (reader) and `prepare_commit` (writer) →
  `formatting_pass_used=false`, no error, valid output; converged; A/B vs the
  pre-change binary shows no regression. Saves one LLM round-trip per
  schema+tools claude_code node across all bots.
- Side: closed a **stale "ready" board ticket** (`native:21065752`, Revi
  "scan_shards.go:458 blocks until shard timeout") — already fixed on HEAD
  by `59cfedcc` + covered by `TestAwaitTerminal_PreDispatchFailureDoesNotHang`
  (passes). A dispatch would have wasted a run on an already-fixed bug.

### Lessons for next run
- **Cost**: `$7.68` to fix 2 lines of incidental drift is high — the 80%
  coverage gate over a **114-file** footprint makes every reviewer pass
  heavy. For a focused ticket, scope `doc_globs` tightly (a 3-file scope
  re-run cost a fraction). `scope_notes` is only a HINT; the mandatory
  full-footprint coverage dominates, so a reviewer can converge on
  incidental drift while leaving the operator's stated focus untouched.
  Consider weighting `scope_notes`-named files into the coverage gate.
- The `md_link` scanner now closes the dead-anchor class; re-run the
  original `c4043495` scope to land the CLAUDE.md:3 / examples.md fixes.

## 2026-06-14 — synthetic clone-validation + 2nd real-bot C082 proof (run 019ec58a)

A second, independent dogfood from the **C082 board-emit** session (parallel
to the real-board run above). Purpose was narrower: confirm Doki's machinery +
convergence on a clean iterion **clone** and, incidentally, exercise the C082
sandboxed-board fix end-to-end on a real catalog bot. (Lower-value target than
the real-board run above — kept for the C082 proof + the gitignore finding.)

- Status: **validated.** Converged with **zero** false fixes (the audited docs
  were accurate).
- Method: dedicated worktree studio :4899 (C082 worktree binary, forfait env),
  clean iterion clone; `doc_globs=docs/resume.md,docs/routers.md`,
  `code_scope_globs=pkg/store/**/*.go,pkg/runtime/**/*.go,pkg/dsl/ir/**/*.go`,
  `merge_into=none`. claude_code/opus + claw `gpt-5.5` forfait.
- Result: **converged in ~2 rounds to a cross-family double-approval**, no
  oscillation. reviewer_gpt audited 18 symbol refs in `docs/resume.md`,
  confirmed them in the Go code, and concluded "No documentation changes
  needed" → `commit_changes` a correct no-op. Correct verification, no
  hallucinated drift.
- **C082: confirmed on a 2nd real catalog bot.** `prepare_commit` (sandboxed
  claude_code, board.create cap) invoked `mcp__iterion_board__create_issue`
  twice through the C082-fixed HTTP transport → board 0→2, real native ids.
  Independent of the minimal C082 validation bot — proves the fix works in a
  real bot.
- Findings:
  1. **`.claude/skills/` runtime mirror not gitignored — FIXED** (`.gitignore`
     `.claude/skills/` + `.docs-refresh-cache.json`). The engine mirrors
     `<bundle>/skills/*.md` into `<workspace>/.claude/skills/` at run start; it
     was uncovered, so it shows as `?? .claude/` and can be swept into a code
     bot's commit (later confirmed live: Bmady's commit included the mirror on
     a clone without this fix). Broader runtime-level exclusion is tracked.
  2. **Empty `code_scope_globs` → every symbol "unverifiable"** (a first
     attempt with `doc_globs` only marked all 22 symbols unverifiable). Always
     pass `code_scope_globs`; an empty default arguably should mean "scan the
     workspace."
  3. Same `StructuredOutput` tool-error as ticket `d8e8dde1` above (non-fatal).
- Lessons: pass `code_scope_globs`; the bot is safe (no false fixes) +
  doctrine-compliant (neutral cache path, flags code issues to the board).
