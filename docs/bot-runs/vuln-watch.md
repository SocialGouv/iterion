# vuln-watch (Senti) — run log

## 2026-08-27 — prod wiring, first live alerts (runs 01a04222 / 01a04232 / 01a04234)

- Status: **validated** — Senti is live on the cloud instance, hourly at `17 * * * *`.
- Versions: bot 0.1.0 · iterion v3.64.1 (prod)
- Method: cloud runs against `SocialGouv/iterion-veille@main`, `mode=watch`,
  `state_commit=true`; credentials from the **watch-only GitHub App** path
  (PR #527), not a PAT.
- Result: the never-exercised App flow works end to end. `poll_dependabot`
  reported `orgs_ok: 2, orgs_failed: 0, new_alerts: 200` — proving the
  `dependabot_tokens` map is keyed by ORG (`socialgouv`, `dnum-socialgouv`)
  and not by the App's bot handle. Run 1 (dry) and run 2 (real) were
  BOOTSTRAP: 0 alerts, 77 observed, cursors armed and state pushed. Run 3
  posted **5 alerts, delivered 5/5** to `#veille-vigie-secu`.
- Value: the five are exactly the shape the design promises — exploitation-driven
  and project-scoped. React Server Components (KEV 2025-12-05, EPSS 100%) named
  **53 projects**; GitLab (KEV 2024-05-01) **17**; WordPress **1** (RITM); plus
  Metabase and a Splunk/Kafka unit. Zero LLM tokens, zero project names sent to
  a model.
- Findings / misses: none new. The 5-alert figure matches the replayed
  acceptance measurement exactly, across a completely different credential path.
- Engine hardening: the watch-only App + `forge.Purpose` seam (PR #527) exists
  *because* widening the runtime forge App to All-repositories would have granted
  `contents:write` on 388 repos to obtain a read. Three adversarial reviewers plus
  one Revi round found 21 real defects in it before merge.
- Lessons for next run: a `dry_run` does NOT commit state, so the first real run
  is always another BOOTSTRAP — budget two ticks before expecting alerts. The
  per-run cap never engaged (0 overflow) on a 5-alert backlog.

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

## 2026-08-24/25 — adversarial review + 4 Revi rounds (23 findings)
- Status: validated
- Method: two opus agents (one per surface: engine flow, bot) with a
  refute-don't-validate posture and mandatory executed proof, then four
  rounds of Revi on the PR. Every finding verified by hand before
  fixing; every fix carries a regression test seen RED by
  re-introducing its cause; the live dogfood replay re-run after each
  round (stable at 5 alerts, identical rendering).
- Result: **23 findings, all real, none dismissed.** The two worst
  classes were both MISSED ALERTS — invisible to a green e2e suite:
  1. *scope-blind suppression*, found at THREE sites in succession
     (`already_alerted`, the intra-run dedup, the re-fire scan). A
     CERT-FR advisory carrying 222 CVE ids sterilised all of them for
     every other project. Fixing two sites and missing the third is the
     lesson: grep the class, not the site.
  2. *cursor granularity* — the KEV cursor compared a day-granularity
     `dateAdded` on an hourly tick; the Dependabot one compared
     inclusively against bulk-created same-second alerts.
- **Two findings were regressions of my own fixes**, which is the
  clearest argument for the loop: the anti-flood rule added for a
  newly-onboarded org swallowed already-exploited alerts permanently,
  and the tenant-scoping fix for the security-read withdrawal was
  invisible to a test whose memory store ignored tenants.
- Security-shaped findings: a decompression bomb (8 MiB → 3 GB in one
  allocation, reachable from the URL a feed itself names), the SSRF
  guard covering one lane of four (a 302 reached the metadata service
  and urllib kept the Authorization header across hosts), untrusted
  titles forging a second indistinguishable alert block, an org-wide
  token surviving a disconnect.
- Lessons for next run: a test double must mirror the PRODUCTION
  contract (the memory secret store ignoring tenants hid a
  cloud-only failure); an anti-noise rule must be stated as "silences
  history, never evidence" or it will eventually silence evidence; and
  a per-finding mutation is the only thing that separates a regression
  test from decoration — two of mine passed against their own defect
  until retargeted.

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
