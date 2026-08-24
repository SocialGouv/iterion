# vuln-watch (Senti) — run log

## 2026-08-24 — retro-Metabase acceptance probe (runs 01a0352e-*, 01a03530-*)
- Status: validated
- Versions: bot 0.1.0 · iterion v3.58.3+7dd9767f6 (feat/vuln-watch-senti)
- Method: real engine (`iterion run`, sandbox none, store in-workspace),
  hermetic workspace at /tmp/senti-probe-metabase with the carto-built
  inventory (409 technologies / 97 projects), NO Dependabot org and NO
  feed configured — KEV + EPSS lanes only, against the LIVE CISA KEV
  catalog and the LIVE FIRST EPSS API. State seeded with
  `kev_date_added: 2026-08-10` (the pre-Metabase-entry cursor) — the
  plan's acceptance test: "replay the KEV diff of 08-11 and the
  Metabase alert must come out, or the bot does not answer the need".
- Result: exactly ONE alert — `CVE-2026-72898` (Metabase SQL Injection,
  KEV 2026-08-11, live EPSS 79%) with the inventory join naming
  DOMIFA + SI-Honorabilité. **Zero false positives** over two weeks of
  KEV additions × 409 inventoried technologies. Zero LLM cost by
  construction. French label templates rendered as designed.
- Findings / engine hardening — the probe found a bug class the unit
  harness structurally cannot see (it reads the LAST stdout line; the
  engine parses the WHOLE stdout as one JSON object):
  1. vuln-watch's dry-run preview prints corrupted the notify output →
     `NO_OUTGOING_EDGE` on the conditional `when consume` edges. Fixed:
     diagnostics to stderr, rendered messages moved INTO the output
     schema (the artifact is the dry-run review surface).
  2. Same latent class in feed-watch (Vigie 1.4.0): the documented
     first-run checklist (`dry_run=true`) dead-ends the same way, and
     the silence-stamp git warnings pollute stdout too. Fixed in 1.4.1.
  3. Uncaptured `git commit` in commit_state leaks git's own summary
     lines into stdout — captured everywhere.
- Lessons for next run: dogfood through the REAL engine before calling
  a bot done — two of three bugs lived exactly in the gap between the
  test harness's stdout contract and the engine's. Consider an engine
  guard (warn when a tool node's stdout carries non-JSON prefix lines)
  as a follow-up.

## 2026-08-24 — first full-config dry-run in the veille workspace
- Status: pending (inventory build in progress — carto + GitHub org
  sweep; will be appended after the run)
