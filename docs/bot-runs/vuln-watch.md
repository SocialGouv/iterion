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

## 2026-08-24 — full-config dry-run, veille workspace, LIVE sources
- Status: validated
- Versions: bot 0.1.0 · iterion feat/vuln-watch-senti (post stdout-fix)
- Method: real engine from the iterion-veille workspace (sandbox none,
  store `$PWD/.iterion`), production config (2 orgs, CERT-FR
  alerte+avis, KEV, EPSS≥0.5, floor=exploited) + the freshly built
  inventory (402 technologies / 212 watched / 207 projects — full
  carto report + 542-repo GitHub sweep). Dependabot tokens = gh OAuth
  set via `iterion secret set --from-env` (removed after). State
  seeded with rewound cursors (Dependabot 08-20, KEV 08-10) to replay
  a real window; dry_run so nothing posts, nothing consumes.
- Result: 45 new Dependabot alerts + 80 CERT-FR publications + 1675
  KEV entries in → **5 alerts out** (React-RSC ALE → 53 projets,
  WordPress → RITM, GitLab → 17 projets, Metabase → CDTN+DOMIFA+
  SI-Honorabilité, Splunk/Kafka → 1 repo), 1 suppressed duplicate,
  20 observed. ~2-3 alertes/semaine au rythme réel — le volume cible.
- Findings (all fixed in the same session, red-first via the replay):
  1. intra-run alias dedup missing — Metabase posted TWICE (CERT-FR
     avis + KEV entry, same tick);
  2. a Splunk avis carrying 222 CVE ids rendered them ALL in the
     message title — display now caps at 4 + "+N";
  3. "Correctif: aucun publié" was false for advisory/KEV units
     (absence of data ≠ no fix) → "voir l'avis" label; signal
     fragments deduped to one per kind;
  4. bootstrap paged the whole open-alert backlog (10 pages/org) for
     nothing — now stops at page 1 (only the newest created_at arms
     the cursor).
- Value: the exploitation-driven policy holds on real data — an
  ordinary week of new criticals stays silent, and the one thing the
  team truly needed to hear this month (Metabase, KEV 08-11) is
  exactly what fires. FP rate on the replay: 1 borderline (Splunk
  Connect for Kafka matched the kafka keyword — the message's real
  title makes it self-explanatory).
- Next: prod wiring (App permission + DNUM install + connection
  opt-in + schedule) — see docs/forge-security-read.md.
