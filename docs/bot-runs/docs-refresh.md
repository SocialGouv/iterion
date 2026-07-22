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

## 2026-07-22 — SANDBOXED cloud validation: converged + PR opened in-pod (runs 019f8a05→019f8a8f)
- Status: validated — the full ADR-082 Phase 3 stack end-to-end
- Versions: bot 2.4.0 · iterion :edge (#268 head) · prod chart runner.sandbox.enabled=true, default image -full
- Method: API launch (bot_id + repo_url SocialGouv/iterion + connection), open_mr=true, iterion surface globs; six iterations, each failure fixed+deployed within ~15-30 min (user-sanctioned CI bypass)
- Result: run 019f8a8f — sandbox pod boots, workflow runs IN-POD, 6 passes (~35 min), gate CONVERGED (docs_aligned ∧ green ∧ coverage), 3 real doc commits (pass 4), finalize_mr pushed from the pod and opened PR #270. Doki 2.4.0 economy confirmed live: verify_precheck reuse on 0-commit passes (~3.5 min vs ~9), chunks of 22 docs (denoised), dismissals ledger advancing every pass.
- Cutover gaps found and fixed by these validation iterations (each observed live, fixed, redeployed):
  1. Runner entry point had no sandbox default (#263 + chart ITERION_SANDBOX_DEFAULT=auto) — cloud runs stayed unsandboxed after the override lift.
  2. Unusable devcontainer (iterion's own --privileged) degraded to unsandboxed forever → default-image fallback at the ambient tier (#264).
  3. k8s driver hard-required sandbox.user → defaults to the published images' 1000:1000 (#265).
  4. git safe.directory: root-owned emptyDir mountpoint vs uid-1000 exec broke repo discovery (exit 128) → GIT_CONFIG_* protected-config env injected pod-wide (#266).
  5. Prod forfait rides the runner pod's AMBIENT env; kubectl-exec spawns only get the SDK env map → claude "Not logged in" in-pod, masked as structured-output schema errors (the known gotcha) → ambient Anthropic env forwarded verbatim for sandboxed spawns (#268).
  6. Board MCP HTTP endpoint not wired on runner-launched sandboxed runs (board handoff degraded to summary) — ticket native:1ec7b869, open.
- Also: cancelled cloud runs resurrected after runner restarts (NATS redelivery ignores terminal status) — ticket native:85cea410, open.
- Lessons: validation-by-real-run caught six gaps no test suite had; "auth failure masquerading as schema error" struck again (test auth FIRST); the argv idiom has a kernel ceiling (ride files); merge-queue drops are silent — verify the gh-readonly-queue branch, and a red gate ON MAIN (OpenAPI drift from direct pushes) starves the whole queue.

## 2026-07-22 — first repo-targeted CLOUD dogfood + PR tail (runs 019f86ac, 019f86ce)
- Status: validated (infrastructure) / low direct value (docs) — the whole cloud PR-tail path + ledger + fixes proven live; zero real doc drift found (all candidates were scanner noise, now denoised in 2.4.0)
- Versions: bot 2.2.0→2.2.1→2.3.1 (runs) / 2.4.0 (post-run) · iterion 37c36f7→aff5e02 (prod :edge)
- Method: PROD cloud instance, studio LaunchView launch (repo-first Target repository = SocialGouv/iterion via GitHub App connection), open_mr=true, cli_surface_globs=cmd/iterion/*.go, diagnostic_surface_globs=pkg/dsl/ir/*.go, defaults 2h/$60/opus-4-8 (forfait runner)
- Result: run 1 (019f86ac, 2.2.0) failed in seconds — build_manifest exit 127; cancelled. run 2 (019f86ce, 2.2.1) ran the full campaign loop: verify gate red pass 1 (verify.sh lacked the CI drift gate → verify_build self-corrected pass 2), then 8 more green-but-not-aligned passes re-judging the SAME 41 cli_flag false positives (the livelock), pass 4 also hit error_max_structured_output_retries (auto-resume recovered). Loop exhausted at 9/9 passes → mark_issue_for_review → update_audit_cache CRASH-LOOPED (E2BIG, bug 3 below) → failed_resumable at 00:21. 9 passes, $14.81, 248k tokens, ~1h48. The PR tail (mr_gate→finalize_mr) was never reached
- Value: the dogfood surfaced 3 real defects (2 engine-adjacent, 1 bot-design) in one evening — exactly what dogfooding is for; the doc-alignment value itself was nil (all adjudicated candidates were cli_flag false positives)
- Findings / misses:
  - ENGINE (space-join): all-string list substituted into a tool command as env assignment executes the 2nd element (`DOC_FILES={{input.doc_files}}` → `bash: README.md: command not found`). Regression exposure since 55d7170e6 (raw-interpolation removal); invisible until now because the 2026-07-20 noop dogfood short-circuited before build_manifest. Bot-side fix: argv section markers (update_audit_cache idiom) — PR #251, bot 2.2.1. Fleet sweep ticket: native:d673c4df (sec-audit langs, rgaa examined_files, adr-cartograph…).
  - BOT DESIGN (livelock): zero-commit adjudication pass → manifest byte-identical → severity-sorted chunker re-surfaces the SAME chunk → run re-judges the same 41 cli_flag false positives every pass (~$1/pass) until max_passes. v2 had lost v1's dismissed-pairs accumulator. Fix: dismissals ledger (campaign records {doc,kind,value,reason} in vars.dismissed_path; build_manifest excludes before sort/chunk; dismissed_excluded telemetry) — PR #254, bot 2.3.0, proven by two-pass emulation (chunk window advances).
  - BOT SCALE (E2BIG): update_audit_cache took verified_pairs INLINE as argv; ~4200 pairs on this repo blew MAX_ARG_STRLEN (fork/exec bash: argument list too long) — crash-looping the terminal node until auto-resume gave up. The argv idiom (the fix for the space-join bug!) has a scale ceiling; large handoffs must ride FILES. Fix: build_manifest writes <scratch>/verified_pairs.json, update_audit_cache reads the path — bot 2.3.1 (same PR #254).
  - BACKEND (structured-output flake): pass-4 campaign died on error_max_structured_output_retries (missing is_code_bug then summary in the termination contract) → failed_resumable → cloud auto-resume relaunched the node fresh. Auto-resume behaved exactly as designed.
  - SCANNER NOISE: cli_flag extraction flags foreign-tool flags in command examples + real iterion flags outside cli_surface_globs — dominant FP source; candidate for a scanner tightening pass (require the doc to document iterion CLI context).
  - CLOUD LIMIT (expected): PROJECT_SCRATCH_DIR is per-run on the runner → inter-run audit cache + noop short-circuit inert on cloud; the new dismissals ledger is also per-run there (works within a run — the livelock fix — but not across runs). Follow-up: per-project persistent scratch on runners.
- Engine hardening: PR #251 (argv fix), PR #253 (async e2e -race flake: 4s ceiling under the 5s level-poll), PR #254 (ledger); the dogfood also motivated ADR-082 sandbox-by-default (Phase 1 shipped, PR #252) after the "Launch without sandbox?" modal question.
- Lessons for next run:
  - Launch catalog bots by bot_id (inline upload strips bundle skills); repo targeting only via studio/API (repo_url+connection_id).
  - The PR tail (mr_gate→forge_auth_probe→finalize_mr) is the ONLY delivery path for repo-targeted cloud runs (ephemeral clone; merge_strategy and runs merge are local-only).
  - A zero-commit "all false positives" pass is a convergence hazard for ANY chunked-manifest bot: persistence of dismissals is part of the termination contract (check adr-cartograph for the same class).
  - Validate with a 2.3.0 run: expect dismissed_excluded > 0 on pass 2+, chunk window advancing, and either real-drift commits + a PR opened by finalize_mr, or an honest 0-commit no-PR finish.
  - VALIDATION RUN (019f874d, bot 2.3.1): the ledger works live — dismissed_excluded 0→40→…→318 across 9 passes, docs_with_drift 196→160, chunk window advancing every pass (livelock dead). Full tail again: update_audit_cache wrote 234 entries via the pairs file (no E2BIG), probe found the forge_token, finalize_mr honestly refused the empty PR (rev-list-verified 0 commits). 9 passes, $16.43, ~1h35, finished. Still zero REAL drift found: candidates were foreign-tool cli_flags + unverifiable symbol_refs — 100% false positives across both runs.
  - BUDGET/PRODUCTION (operator finding): ~$31 / ~3h20 for zero doc fixes is a terrible ratio. Root causes and fixes shipped as bot 2.4.0 (PR #256): deterministic verify_precheck (skip the ~5-6min/~1$ verify chain when HEAD unchanged since last green verify — 8 of 9 passes qualified), foreign-flag heuristic (drifted cli_flag lines that reference no known CLI command → unverifiable), unverifiable symbol_refs no longer surfaced by default (include_unverifiable_symbols=true to opt in). Expected effect on this repo: adjudication-only passes drop from ~9min to ~3-4min and the candidate pool shrinks to real drift.
  - Studio replay pane shows "No log captured." for these cloud runs — diagnosis/fix delegated (separate PR).

## 2026-07-07 — v2 dogfood on iterion's own docs: 4 real fixes in stride, honest non-convergence on manifest FPs, self-filed findings (run 019f3d4d-1aed)
- Status: **VALIDATED** — every v2 mechanism behaved as designed over 3 passes, including the honesty-under-chunking clause and the findings handoff; the residual is manifest-heuristic quality, filed as issues.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8` · no sandbox, worktree: auto.
- Method: CLI run, `--store-dir <workspace>/.iterion`, `--merge-into none`, `diff_since=origin/main`, `bundle_self_path=bots/docs-refresh`, `max_passes=2`, `--max-cost-usd 20 --max-duration 1h`. 27m32s wall, 3 campaign passes (NB: `as loop(N)` bounds LOOP-BACKS, so N=2 ⇒ up to 3 passes — wording fixed across the v2 bots after this run).
- Result: `finished` (loop exhausted → ship-what-is-banked). Scan: 197 docs, 3592 anchors, 48 mechanically drifted, coverage 97%. **Pass 1: 4 real doc fixes, one commit each in stride** on `iterion/run/ash-throb-laserdoom-aa99` (dead ADR-010/ADR-053 links after renames, dead pkg/ links in the totality doc, goldens scenario list aligned with the Nexie v2 rewrite). Passes 2-3: the re-manifested chunks were **100% heuristic false positives** — the campaign verified each at the anchor (ls/find evidence), refused to "fix" non-drift, reported `docs_aligned=false` with `commits_this_pass=0`, and did NOT rubber-stamp under chunking (106 docs still deferred). scope_check clean ×3; verify green ×3; cache rewritten with **178 mechanically-verified entries** (the new verified_pairs path); mark_issue no-op (no issue_id).
- Value: real doc repairs landed + the run itself produced its follow-up work: the campaign **self-filed** the manifest-bug finding (native:cbf91e1f — .tsx→.ts truncation ×17, repo-root-relative link base, [link-text] vs ](target) extraction) after checking the inbox first and REFERENCING the operator-filed dismissed-pairs improvement (native:8c6dc311) instead of duplicating it.
- Findings / misses: build_manifest's extractor needs the 3 fixes above (severity low — FPs cost passes, not correctness); without persisted dismissals each pass re-adjudicates the same FPs (native:8c6dc311).
- Engine hardening: none needed by this run.
- Lessons for next run: on a healthy doc tree, `max_passes=1` (2 passes) suffices — extra passes only re-judge FPs until the manifest fixes land; `diff_since=origin/main` correctly prioritised the fresh drift.

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
