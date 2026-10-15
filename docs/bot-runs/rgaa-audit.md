# Acci — `rgaa-audit` run bilans

Accessibility auditor (read-only) on the pinned ultra11y engine since v3
(2026-10-13, PR #2387): the engine finds, gates and reports; read-only agents
adjudicate the criteria a static pass cannot decide; a second reader puts the
draft report on trial; the run writes the dated French report, a coverage
annex and the issue plan. See [bots/rgaa-audit/](../../bots/rgaa-audit/).
The bilans below v3 are the v1/v2 single-agent auditor, kept as the record
of why v3 exists. Earlier runs (pre-dedicated-file) are recorded in the
2026-06 campaign bilans.

## 2026-10-14 — r3 and r4: the first complete studio audit, its ledger replayed, its trial run (runs 01a13b95-2e46, 01a13bf2-0e79)

- Status: **validated end to end, board publication not exercised** — r3 decided all 106
  criteria; r4 replayed r3's ledger and ran the second reader, which found the defect fixed
  here (3.0.2).
- Versions: bot rgaa-audit 3.0.1 (#2439) · iterion `3f786feac` (r3), main `13cb0edcf` plus
  r3's ledger (r4) · ultra11y 5.42.1 · the engine's Chromium 1248 · adjudicator and second
  reader on `claude_code` / `claude-opus-5-5`.
- Method: the studio recipe of #2439; `--loop-budget-guard off`; r3 capped at 60 $ on the
  34-verdict ledger of r2; r4 capped at 30 $ on r3's ledger, committed on a local branch first
  (a run audits the worktree of `HEAD`).
- Result, r3: **41.90 $** for 21 adjudication calls (0.14 to 5.62 $ a batch, 2.8 min on
  average) and 22 min.
  - Ledger: 32 verdicts replayed, 2 stale.
  - Adjudication: 17 batches and 4 re-asks, none lost. The fold accepted all 74 verdicts
    and refused none.
  - Recommendations: all 22 kept and folded, none refused. r2 lost 18 verdicts of 54 to them
    (#2398, closed).
  - Report: **106/106 decided — 41 C, 32 NC, 33 NA, 56 %**, not provisional; integrity ok.
  - The second reader was **skipped** by `refuter_budget_share`: 0.6 × 60 $ = 36 $, already
    spent when the trial would start.
  - Issue plan: 47 issues, 15 of them advisory, none blocking.
- Result, r4: **20.39 $** — adjudication 8 calls for 11.91 $, second reader 13 calls for
  8.48 $ — and 15 min.
  - Ledger: 93 verdicts replayed, 22 stored recommendations not replayed, 10 stale. All 10
    stale ones are anchored on page captures: the rendered DOM differs from one boot of the
    studio to the next on the same source.
  - Adjudication: the 10 stale criteria were re-adjudicated, 1 needing the re-ask, and none
    was left open.
  - Trial: 123 claims (87 NC, 36 C). 1 refuted and 2 unsupported, all three NC; every
    conformity was supported or partly supported. The engine removed 2 agent findings and
    left 1 of its own untouched.
  - Report: **106/106 — 38 C, 35 NC, 33 NA, 52 %**; integrity ok.
  - Issue plan: 39 issues, 4 of them advisory.
  - Ledger: 102 verdicts, committed here as the studio's baseline.
- Value: the first complete audit of the studio. Replaying the ledger halves the cost of a
  run (r4 20 $, r3 42 $) and leaves the budget for the trial.
- Findings / misses:
  - **the trial drew its conformities from the ledger copy, while the engine re-derives them
    from the audit when it applies the trial**. On a replaying run the two write a capture
    anchor differently (relative in the ledger, the run's absolute worktree path in the
    audit), so 16 conformities read « absent from the trial ». The second reader had
    supported or partly supported all 16, so r4's ledger is checked on substance; only the
    bookkeeping missed. Fixed in 3.0.2: `draft_report` takes the conformities from the audit.
    The node test fails on the old source with 77 claims absent; on r4's own data, an
    audit-drawn trial leaves none absent.
  - **r4's trial was degraded by a second cause, which 3.0.2 does not remove**: the
    conformity of 1.3 cites `BackendsTab.tsx:104` with the adjudicator's retyping of the
    harvester's snippet — a closing backtick after `shrink-0`, where the source reads
    `shrink-0 ${` (the element spans lines 104–110). The fold accepts it through its
    evidence-anchor fallback; the trial grounds retained claims without one and refuses it.
    The committed ledger carries that citation, so every trial that re-judges 1.3 comes back
    degraded the same way, until the engine aligns the two (SocialGouv/ultra11y#50) or the
    bot gives a citation its anchor's own snippet (#2453).
  - the second reader's budget share was spent before the trial: at a 60 $ cap, r3 had
    already spent 42 $ adjudicating 74 criteria (32 came replayed from r2's ledger).
  - **captures churn between boots**: 10 of 103 replayed verdicts went stale on unchanged
    source. A weekly run re-adjudicates about 10 % unless the served studio pins what its
    pages show (#2347).
  - after the trial pruned two findings of 10.7, the page grid leaves 10.7 « à évaluer » on
    18 pages. The document decides it NC, and the annex does not name the page gap (#2449).
- Lessons for next run: adjudication costs about 0.57 $ a criterion here, so a studio run
  with no ledger (103 criteria, about 58 $) needs a cap near 100 $ for the second reader to
  start at the 0.6 share; a run replaying this ledger costs about 20 $ with the trial.

## 2026-10-13 — first dogfood on iterion's studio (#2346): the rendered layer proven, 30 $ short of 19 batches, four bot defects found and fixed (run 01a13705-d54a)

- Status: **partial, degraded and saying so** — the whole graph ran on the real studio
  (static, rendered, adjudication, trial, report, per-page grid, annex, issue plan), but the
  cost cap cut 7 of the 19 adjudication batches: 69 of the 106 criteria stay « à évaluer » and
  the rate is provisional. Board publication not exercised (`post_to_board=false`).
- Versions: bot rgaa-audit 3.0.0 plus this branch's `render` fix (run base `0d506c744`); the
  trial ran on the old `refute_apply`, and its fix, like the recommendation and ledger fixes
  below, landed after the run and is proven by the node tests, not by this run · iterion
  `116120b35` (local build) · ultra11y 5.42.1, pinned and fingerprinted · the engine's
  Playwright Chromium 1248, installed under the project (`PLAYWRIGHT_BROWSERS_PATH`) ·
  adjudicator and second reader on `claude_code` / `claude-opus-5-5`.
- Method: the studio recipe this bilan ships — `.ultra11yrc.json` (21 pages with the view
  files as `sources`, 3 transverse components) and `studio/e2e/serve.mjs` with
  `ITERION_E2E_FIXED_RUN_IDS=1`, so the seeded run ids survive a reboot and `/runs/<id>` can
  be sampled — then `iterion run bots/rgaa-audit --sandbox none --merge-into none --var
  scope_globs='studio/src/**' --var rendered=required --var serve_command='… node
  studio/e2e/serve.mjs' --var serve_ready_url=http://127.0.0.1:4899/api/server/info
  --max-cost-usd 30`; resumed once at 60 $ (`iterion resume --run-id … --max-cost-usd 60`).
  The resumed run died `AUDIT_INTEGRITY` at `final_report` (the old `refute_apply` had not
  recorded the engine's rewrite of the audit); the hash record was realigned on the engine's
  write by hand and the run rewound to `final_report` (`iterion rewind --node final_report` +
  `resume --force`).
- Result: **35.36 $** over 24 model calls — adjudication 14 calls, 27.71 $ (0.29 to 3.78 $
  for a batch of at most 8 criteria, 5 min on average), second reader 10 calls, 7.65 $ — and
  26 min of run time over three segments (40 min from launch to finish, pauses included).
  - Static: 553 files, 15 engine findings, 100 criteria left to judgement.
  - Rendered: 21 pages captured, none refused; the page-kind lint names the 6 kinds the studio
    does not have (contact, legal notice, accessibility statement, site map, help,
    authentication — the last one cloud-only).
  - Adjudication: 12 batches answered (54 verdicts); 7 lost — 4 cancelled when the cap was
    hit, 1 adjudicated but stopped at the cap before its fragment was written, 1 on a
    claude-code `error_during_execution`, 1 never started. Fold: 33 verdicts accepted, 67
    criteria still open — 49 from the lost batches, **18 refused for a single recommendation
    without a file** (#2398).
  - Trial: 95 claims, 20 refuted, 2 unsupported; the engine cleared 2 conformities (3.1,
    11.12) and refused the grounding of 9 retained claims anchored on page URLs. The
    delivered annex still says « the engine did not apply the trial »: that is the misreport
    fixed below.
  - Report: its own line reads 10 C, 11 NC, 16 NA, 69 to assess — **11 % provisional**
    (10 ÷ 90). The bot's recount of `criteria[]` gives 12 C and 13 %: two subject-less
    criteria left open (13.3, 13.4) are counted both « sans sujet » and « à évaluer » by the
    engine (SocialGouv/ultra11y#42), and the annex reports the integrity layer as degraded.
  - Issue plan: 12 issues, severity-sorted — 11.1 blocking (18 occurrences), 7.1 and 10.7
    (28 occurrences each) among the major ones.
  - Ledger: 34 verdicts, committed as `.ultra11y/verdicts/rgaa.json` for the next run to
    replay through the engine — the run's own ledger with its 56 capture anchors rewritten
    relative to the workspace after the run, the form `final_report` now delivers.
- Value: the rendered layer works end to end on a real SPA (served by the e2e harness,
  sampled, scanned, merged before adjudication); the annex names every degraded layer with
  its cause; the issue plan is the engine's own, capped and severity-sorted.
- Findings / misses — four bot defects, fixed in 3.0.1 with a test seen red on each:
  - **a recommendation cost its verdict**: `write_fragment` kept only string recommendations,
    while the engine requires an object anchored on a file and refuses the whole verdict over
    a single free-text one. Every recommendation was either lost (an object) or fatal (a
    string). They are now objects kept only on an anchor of the criterion's own evidence,
    with that anchor's snippet so they ground; the rest are dropped and counted in the annex,
    and the prompt and the adjudication skill state the shape. A stored recommendation is not
    replayed from the ledger either: the engine re-grounds it with no fallback, so an
    advised element since edited would refuse the replayed verdict; and a replayed verdict
    the engine's gate refuses is now named instead of being counted as replayed. The
    adversarial review of these fixes hardened the replay further: a ledger the engine
    cannot read is `LEDGER_INVALID` with the engine's own message, a failure of the
    environment stays a resumable error, and the replayed audit is written beside the real
    one and moved into place once whole.
  - **the trial report contradicted the audit**: with `--prune` the engine rewrites the audit
    and repairs the ledger whatever its verdict on coverage and grounding. The node chained
    the hash only on `ok` (→ `AUDIT_INTEGRITY` at `final_report`) and reported « not applied »
    while 3.1 and 11.12 had been cleared. It now chains the engine's write, reports what was
    pruned and names the part the engine refused — in the annex and at the human gate.
  - **a refused page-sample lint read as « no kind missing »**: `render` now fails the layer
    on it, before any scan, with the lint's own issues as the reason.
  - **the delivered ledger carried this run's absolute workspace paths** (56 capture
    anchors under the run's worktree): `final_report` now writes every anchor relative to the
    workspace, and a capture from any run's worktree from its `.ultra11y/pages/`, the rule
    the engine's replay applies.
- Engine (upstream): the trial grounds a page URL as a file, so every trial of a rendered
  audit comes back refused in part (SocialGouv/ultra11y#49); the « Décidés » double count
  stays (SocialGouv/ultra11y#42).
- Engine facts paid on this run:
  - a local run executes in the worktree iterion creates from `HEAD` (`worktree: auto` is
    the default): the first attempt died `RENDER_UNAVAILABLE` on a recipe not yet committed;
  - on a resume, `loop_budget_guard` prices the re-ask at the previous iteration — the whole
    19-batch fan-out, 27.7 $ — and declined it, although the re-ask replans only the refused
    criteria;
  - a local `claude_code` run inherits the shell's provider keys: a `ZAI_API_KEY` in the
    environment routes it to the z.ai facade.
- Lessons for next run: about 2 $ a batch, so a cold studio run is 19 batches + one re-ask +
  the trial, about 50–60 $; the committed ledger replays 34 verdicts for free and leaves
  about 9–12 batches. Pass `--loop-budget-guard off` on a resume. Commit the recipe and the
  ledger before launching.

## 2026-10-13 — v3 on the ultra11y engine lands (PR #2387); Ally (`ultra11y`) retired into it

- Status: **structural + engine-proven, dogfood pending** — the real node scripts run on
  the real pinned engine against the planted fixture site in CI (`task test:contract:rgaa`:
  the engine contract and the bot's node scripts, 45 subtests — hostile first round, tamper
  table, re-entry), the dry run walks the graph
  on recorded fixtures, `task check` green; no live run yet (that is #2346, the studio
  dogfood, after the live test).
- What the three Ally bilans taught (2026-08-12 → 2026-09-17), now built into v3:
  - the empty-scope façade (`audit_args` joined into one token → 0 files → "100 %"):
    `audit_args` is `string[]` and an empty scope is the typed refusal `EMPTY_SCOPE`,
    never a clean audit;
  - the fold refuses W3C technique codes as `normativeRef`: the briefs the engine writes
    per criterion carry the test ids, and the adjudication protocol says "the criterion id
    or one of its `testIds`, never a technique code";
  - a report written inside the tree gets wip-banked by finalize: every intermediate
    lives in the run directory — except the captures the engine's `scan` writes under
    `.ultra11y/pages/` of the workspace (upstream SocialGouv/ultra11y#47) —, `report_dir`
    is an explicit copy, `workspace_checkpoint: off`;
  - one adjudicator on a whole SPA does not converge in 20 minutes: criteria are
    adjudicated in parallel theme batches, each with its own agent;
  - the "plausible C" hole (a C with a justification but no verified anchor): a C must
    cite the item's own evidence (checked by the bot before the engine's gate), and a
    second reader puts every landed conformity on trial;
  - undecided criteria are never silently conforming: the fold is partial, the residue is
    named in the annex, and the report's « Décidés » line is cross-checked against the
    document (the engine double-counts "no subject" criteria left open — known upstream
    defect, pinned by a test that goes red when fixed).
- Not carried over from Ally: the pull-request diff mode (`--since`) and `produces:
  review` (the hand-off would mask Revi on the same PR, #2355) — back when #2355 lands.

## 2026-09-17 — profile 2: the 106 criteria ruled on a 13-line page, three attempts of wall to get there (run 01a0af95-64fa)

- Status: **validated end to end** — `inventory` → `campaign` (cut twice by the wall, then
  complete) → the deterministic tail (`scan_health`, `cap_findings`, …) → `report_card` →
  done.
- Versions: bot rgaa-audit 2.0.1 (`dsl: 2`, wave 3b of #1344) · iterion `5fa790d82` for the bots; the engine the branch binary `v3.157.0+c31559517` (built on the wave-3a branch from main at v3.157.0; main is at v3.159.0 as this is written, one PR of engine work ahead), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records.
- Method: launched FROM a scratch greeter repository whose `web/index.html` carries planted
  defects, `--var scope_globs="web/**" --var post_to_board=false` plus a one-line
  `scope_notes`, `--store-dir` the
  operator's workspace store, `--sandbox none`, `--merge-into none`, caps
  `--max-cost-usd 1.5 --max-duration 10m`, every LLM node on `claude_code`/`claude-opus-5`
  (the GPT forfait is closed until 2026-09-20). The single audit agent died at the 10-minute
  wall (context deadline); resumed with `--max-duration 25m --max-cost-usd 2.5`, the campaign
  then finished but the 90 % duration guard fired on the deterministic tail; resumed once more
  with `--max-duration 45m --max-cost-usd 3`.
- Result: $2.33 recorded (the two cut attempts' spend is not), 29 min 45 s from launch to done.
  The campaign audited the page theme by theme against the 106 RGAA 4.1.2 criteria without
  the DSFR skill (no framework on the page, said explicitly): 16 C / 15 NC / 75 NA, 31
  applicable, 52 % — the planted defects all caught, with the sibling-criterion rule the
  agent chose written down (a criterion whose object does not exist because a sibling failed
  on absence is NA with a reason). `report_card` wrote `audits/rgaa-2026-09-17-2.md`; the
  `-2` suffix because a cut attempt had already left `rgaa-2026-09-17.md` in the tree.
  `report_dir` defaults to `${PROJECT_DIR}/audits`, so finalize wip-banked both as
  `7fd2c33` on `iterion/run/01a0af95-64fa-7ab3-8aa9-07925fad26c4` (never merged, by design).
- Value: the four prompts' paragraph breaks reach the models (proven in the run's
  `events.jsonl` for `campaign_system`, twice, and `report_card_system`), and the report is
  the operator-facing French document the bot promises.
- Findings / misses: none bot-side. The one-agent audit of a full referential needs about
  twenty minutes of wall even for a one-page scope; a cut attempt leaves its half-written
  report in the tree and the next one numbers around it — harmless, but worth knowing.
- Engine hardening: none needed.
- Lessons for next run: `--max-duration 30m` from the start; `--var report_dir=<outside the
  tree>` when the report is the deliverable.

## 2026-07-07 — re-run post status-fix: the 5 NC now flow through the gates into the report — VALIDATED (run 019f3de8)
- Status: **VALIDATED** — closes the morning run's partial: with the d6b966f03 fix live (mandatory per-candidate status + fail-safe NC counting), the same scope now yields scan_health nc_count=5 and a report with full detailed NC blocks.
- Versions: bot v2.0.0 (+status fix) · iterion `dev+239203525cc8` · no sandbox.
- Method: identical scope to 019f3d3b-7aea (studio/src/**, post_to_board=false, report out-of-tree), 6m13s wall.
- Result: `finished`; 22 files examined; **nc_count=5** through scan_health AND cap_findings; report `rgaa-2026-07-07-2.md` (incremental suffix worked) carries the detailed non-conformities with file anchors + corrections — [NC] 11.10 aria-invalid on validated fields, [NC] 5.7 table header scope, [NC] 5.4/5.5 captions, etc. — and an honest 72% conformity over 18 applicable criteria (vs the broken run's 93%/0-NC façade-adjacent output).
- Value: a genuinely actionable a11y worklist for the studio SPA; the run also demonstrates the fail-safe gate philosophy (surfacing beats dropping) end to end.
- Lessons: with post_to_board=true these 5 NC would land as labelled board issues — enable it on the next scheduled run.

## 2026-07-07 — v2 dogfood on the iterion studio SPA: coherent report, status-field gate hole found + fixed (run 019f3d3b-7aea)
- Status: **partial** — the run finished clean and produced a genuinely useful report, but a bot bug dropped its 4 concrete findings between the campaign and the report. Bug fixed same-day (see Engine/bot hardening); needs one re-run to be **validated**.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8` · no sandbox (read-only host run).
- Method: CLI run, `--store-dir <workspace>/.iterion`, `post_to_board=false`, `scope_globs=studio/src/**`, `report_dir` out-of-tree, `--max-cost-usd 12 --max-duration 45m` (mono claude, forfait). Single pass, ~8 min wall.
- Result: `finished`; scan_health green (5 skills present, 29 files examined — the anti-façade floor held); report written (`rgaa-2026-07-07.md`, 4.1K): 51 statically-verifiable criteria all C, 15 NA with reasons, 93% global with an HONEST method note (render-only-verifiable residual left "à vérifier visuellement" instead of inflated to C), 11/13 themes covered, iframe/media themes NA with reasons.
- Value: real — the report's synthesis table + the campaign's praise of the a11y-mature primitives (IconButton aria-label enforcement, InlineBanner role=alert, skip-link + landmarks) match the codebase; the method note shows the honesty clause working.
- Findings / misses: **the run's 4 concrete findings (5.7 table header scope, 5.4/5.5 missing captions, 11.10 aria-invalid, +1) were emitted WITHOUT a `status` field** → scan_health/cap_findings (which count `status=='NC'`) saw 0 NC → the report says "0 non conformes" while finding-shaped candidates existed. Signal lost between campaign and report — the exact façade-adjacent hole the dogfood ritual exists to catch.
- Engine/bot hardening: same-day fix in `bots/rgaa-audit/main.bot` — (1) contract now REQUIRES an explicit `status: C|NC|NA` per candidates[] entry; (2) scan_health + cap_findings became fail-safe: a finding-shaped entry (severity/problem present) with a missing/unknown status COUNTS AS NC (surfacing beats dropping). `iterion validate` clean.
- Lessons for next run: re-run on the same scope to confirm the 4 findings surface as NC blocks + (with post_to_board=true) as board issues; consider adding `status` presence to scan_health's hard-fail conditions if the fail-safe proves noisy.

## 2026-07-07 — converted to v2 light shape (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only (`iterion validate` clean, catalog tests green).
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07).
- Shape: the v1 detect_ui (claw) → rgaa_review (claude_code) split collapsed into ONE campaign auditor (surface classification = its phase-0 opening move; one session, DSFR MCP from turn one). 6 nodes → 5 exec; the deterministic anti-façade machinery (inventory, scan_health hard-fail, cap_findings, report_card) is unchanged.
- Next: a live dogfood on a DSFR UI repo + bilan here.
