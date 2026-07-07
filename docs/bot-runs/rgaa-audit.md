# Acci — `rgaa-audit` run bilans

Universal RGAA 4.1.2 accessibility auditor (read-only): classify the UI
surface, audit theme-by-theme against the 106 criteria, write the dated
conformance report and (optionally) file one board issue per
non-conformity. See [bots/rgaa-audit/](../../bots/rgaa-audit/).
Earlier runs (pre-dedicated-file) are recorded in the 2026-06 campaign
bilans.

## 2026-07-07 — converted to v2 light shape (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only (`iterion validate` clean, catalog tests green).
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07).
- Shape: the v1 detect_ui (claw) → rgaa_review (claude_code) split collapsed into ONE campaign auditor (surface classification = its phase-0 opening move; one session, DSFR MCP from turn one). 6 nodes → 5 exec; the deterministic anti-façade machinery (inventory, scan_health hard-fail, cap_findings, report_card) is unchanged.
- Next: a live dogfood on a DSFR UI repo + bilan here.
