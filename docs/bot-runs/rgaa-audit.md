# Acci — `rgaa-audit` run bilans

Universal RGAA 4.1.2 accessibility auditor (read-only): classify the UI
surface, audit theme-by-theme against the 106 criteria, write the dated
conformance report and (optionally) file one board issue per
non-conformity. See [bots/rgaa-audit/](../../bots/rgaa-audit/).
Earlier runs (pre-dedicated-file) are recorded in the 2026-06 campaign
bilans.

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
