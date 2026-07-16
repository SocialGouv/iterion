# app-dev (Appy) — dogfood runs

Newest first. Template: see [README.md](README.md).

## 2026-07-16 — triple maiden run: autonomous NextJS+DSFR, interview→CLI, brownfield headless (runs 019f69a7 / 019f69a8 / 019f69b3)

- Status: validated (all three runs)
- Versions: bot 0.1.0 · iterion 34da65370 (worktree branch, pre-merge)
- Method: claude_code + opus-4-8 everywhere, sandbox-full:edge, host `~/.claude`
  OAuth mount, `--store-dir <repo>/.iterion` (operator studio visibility),
  `--merge-into none`. Three complementary scenarios:
  1. **autonomous** (019f69a7): empty non-git `/tmp` fixture,
     `app_prompt` = annuaire des administrations Next.js + DSFR
     (recherche, page détail, seed JSON, smoke test), `stack=nextjs-dsfr`.
  2. **interview** (019f69a8): empty fixture, `mode=interview`,
     near-empty prompt ("Un petit utilitaire CLI"), answers given via
     `iterion resume --answer message=…`; converged spec = `paristime`
     (heure Paris/UTC, stdlib only).
  3. **brownfield headless** (019f69b3): re-run of app-dev on the
     interview fixture (now a git repo), `draft_review=false`,
     `--max-cost-usd 15`, evolution prompt (option `--zone=<tz>`).
- Result: **all converged pass 1** (each request_changes round also
  reconverged in 1 pass).
  - autonomous: 6 commits pass 1 (`chore(scaffold)` → `feat(skeleton)`
    DSFR header/footer + smoke → `docs` README/ADRs → `feat(annuaire)`
    recherche + liste → détail + breadcrumb), verify green on real
    `npm run build` + 5 node tests (dont 404), review clean,
    draft_review pause with literal how_to_run; request_changes
    (« page /mentions-legales + lien footer ») → 1 more pass (2 commits:
    `feat(legal)` + `docs`) → reconverged → ship. $4.73 / 88.6k tokens /
    ~28 min wall (including both human pauses).
  - interview: 2 interviewer turns, **same claude session across turns**
    (sid 9a81d870… on both — the `_session_id` loop mapping works),
    `docs(spec): SPEC.md from operator interview` committed BEFORE any
    code, campaign shipped the CLI + tests, request_changes
    (`--format=iso`) → dedicated commit + 6 tests green → ship.
    $2.13, ~9 min of active time end-to-end.
  - brownfield: **no re-scaffold** (marker detection worked), 1 commit
    (`--zone` + tests), worktree ACTIVE this time (repo exists):
    commit banked on `iterion/run/atomic-bound-sonarsnoot-81d5`,
    fixture's checked-out `main` untouched (merge-into none), headless
    edge exercised (`draft_gate → mr_gate → done`, no human pause).
    $1.38, ~5 min.
- Value: the full product promise demonstrated in one afternoon — spec
  interview with real conversational memory, free-first-draft that
  builds+tests a real DSFR app, operator reframing loop, and safe
  evolution re-runs. The generated annuaire was **verified in a real
  browser (Playwright)**: DSFR banner + landmarks, labelled search
  (`?q=` shareable), filtered results (3 « mairie »), detail page
  (breadcrumb, adresse, horaires, contact `tel:`/`mailto:`, external
  link with « nouvelle fenêtre »), real 404 on unknown slug, legal
  notice page after the reframe. The paristime CLI was executed by
  hand (Paris/UTC/ISO/zones all correct).
- Findings / misses:
  - The campaign spontaneously produced ADRs (stack + data decisions)
    and a footer « Accessibilité : non conforme » declaration — the
    contract's ADR clause and the DSFR skill both landed.
  - Interview convergence is efficient but trusting: a terse operator
    answer ("on y va") converges immediately — fine for an expert,
    worth watching with less-specified briefs.
  - `verify_probe` correctly forced verify.sh regeneration on the
    request_changes pass (iteration semantics reset per resume burst) —
    costs one extra verify_build (~$0.22) per reframe round; acceptable,
    could be optimized later.
  - Store layout note: run artifacts now live under `artifact_files/` +
    `turns/` (not `artifacts/<node>/<v>.json` as older docs say) —
    session-id checks must read `events.jsonl`.
- Engine hardening: none needed — no engine bug surfaced across the
  three runs (in-place degrade on non-git fixture, sandbox bind-mount
  writes, pause/resume cycles, loop bookkeeping
  `continuation_loop=0;draft_loop=1`, and worktree finalization all
  behaved per contract).
- Lessons for next run: (1) keep dogfood briefs small — the three-run
  matrix cost <$10 total; (2) for the studio-first UX test, launch via
  CLI against the workspace store and answer gates in the studio until
  the branch is merged (the main studio only discovers bots on its own
  tree); (3) consider a `draft_review` quick-reply preset ("ship") in
  the studio form to reduce gate friction.
