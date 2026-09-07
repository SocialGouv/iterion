# vuln-watch (Senti) — inventory-scoped vulnerability sentinel, zero LLM

🛡️ Polls the security sources hourly, matches them against the technology
inventory the target workspace maintains, and posts an actionable chat alert
for exactly the vulnerabilities that are **exploited** and that touch
something the inventory says you run.

The compiled workflow contains **no agent and no judge node**: a run can
neither spend a token nor show a project name to a model. Every decision is
deterministic.

## When to use it

Use it to watch published vulnerabilities (CVE/GHSA, CERT-style advisories)
against a maintained inventory of the technologies and projects an
organisation actually runs, with exploitation-driven noise control.

It is **not** an editorial news digest (that is `feed-watch`/Vigie), **not** a
PR dependency gate (`supply-shield-cve`), and **not** a code audit
(`sec-audit-source` / `sec-audit-deps`). It never edits code.

Requires the target workspace to carry a `vuln-watch.json` config and an
`inventory.json` — both described in
[`skills/senti-config.md`](skills/senti-config.md).

## How it runs

One linear deterministic pass, eight tool nodes:

```
plan → poll_dependabot → poll_advisories → poll_exploit → match_policy
     → confirm_versions → notify → commit_state
```

Three detection lanes feed one policy decision:

- **`poll_dependabot`** — GitHub org Dependabot alerts (library-level, per
  repo): new alerts grouped by advisory, repos joined to inventory projects.
- **`poll_advisories`** — advisory feeds (CERT-FR-style): new publications
  matched word-boundary against the inventory technologies, using CERT-FR's
  structured per-publication JSON (cves + affected systems) when available.
- **`poll_exploit`** — the anti-noise core: CISA KEV (diff + join) and EPSS
  scores.

`match_policy` alerts **only on an exploitation signal** (a KEV entry, an
alert-class advisory, or EPSS ≥ threshold). An ordinary new critical stays
silent, is recorded in an observation window, and **re-fires** the day its
exploitation signal lights up. Dedup is deterministic over CVE/GHSA alias
sets held in a seen state; message wording is label-templated, so any
language is a config change.

Source failures are explicit rather than silent: a configured org with no
usable token **fails the run**, and a source silent longer than
`source_stale_hours` is announced on the sinks.

## Configuration

The workspace's `vuln-watch.json` is the real configuration surface (sources,
sinks, thresholds, labels) — see [`skills/senti-config.md`](skills/senti-config.md).
The `.bot` vars below are the run-level dials:

| Var | Default | What it does |
|---|---|---|
| `mode` | `watch` | Single mode today; the enum leaves room for future ones |
| `dry_run` | `false` | Prepare and print every message and payload, deliver nothing, persist nothing — the next real run recomputes the same alerts |
| `config_path` | `vuln-watch.json` | Workspace-relative config |
| `inventory_path` | `inventory.json` | Fallback inventory path; the config's own `inventory_path` wins |
| `state_dir` | `.vuln-watch` | Workspace-relative state root (seen sets, cursors, health, alert log) |
| `state_commit` | `false` | Commit (and push) the state dir after each mutation. **Required on ephemeral runners**; needs `state_dir` NOT gitignored, plus push credentials |
| `max_alerts_per_run` | `20` | Per-run cap on posted alerts. Overflow units stay observed (they re-fire next run) and the overflow is announced — never a silent truncation |
| `observe_window_days` | `60` | Days a non-alerted CVE stays in the observation window, re-scored against KEV/EPSS every run |
| `kev_max_age_days` | `90` | A KEV entry older than this is never treated as fresh, bounding the blast radius of a lost or corrupted cursor |
| `source_stale_hours` | `24` | Hours without a successful poll before the silence is announced on the sinks (at most once per window). `0` disables |
| `max_version_lookups` | `40` | Ceiling on the lookups version confirmation may spend in one run |
| `max_version_seconds` | `120` | Wall-clock deadline on the same node — a *count* is not a time bound. `0` disables |
| `fetch_timeout_secs` | `20` | Per-request HTTP timeout |
| `allow_private_sources` | `false` | Strict by default: http/https only, and any host resolving to a private / loopback / link-local / cloud-metadata address is refused up-front **and on every redirect hop**. `true` relaxes it for trusted on-prem deployments and hermetic tests only |

`max_version_seconds` exists because `confirm_versions` sits upstream of both
`notify` and `commit_state`: overrunning the budget there means no message
delivered and no state consumed, so the bot would go silent for exactly as
long as the forge is slow — plausibly when it matters most.

## Invocation

The manifest declares one **scheduled** invocation (`mode: direct`, suggested
cron `17 * * * *`) — the hourly watch.

```sh
# Dry-run: build every alert and print it, deliver and persist nothing.
iterion run bots/vuln-watch/main.bot --var dry_run=true

# Hourly watch, versioning the state in git (required on ephemeral runners).
iterion run bots/vuln-watch/main.bot --var state_commit=true
```

Org-wide Dependabot alert reads need the `dependabot_tokens` team secret and
the right App permissions — including the coverage trap where a
`selected`-scope installation is silently near-blind. See
[docs/forge-security-read.md](../../docs/forge-security-read.md).

## Notable

- `worktree: none` — the state *is* the product and must persist in the
  workspace across runs; a run worktree would isolate the writes and discard
  them at finalization.
- `repo_devbox: off` — the bot reads two JSON files and talks to security
  APIs. Realising the target repo's toolchain would buy nothing and slow the
  hourly tick.
- `budget.max_cost_usd: 0.5` is a belt only: the workflow has no LLM node to
  spend anything.
- `max_duration: 15m`, `max_parallel_branches: 1`.

## Skills

- [`skills/senti-config.md`](skills/senti-config.md) — the `vuln-watch.json`
  and `inventory.json` contract: sources, sinks, thresholds, label templates.

## Run history

Dated bilans live in [docs/bot-runs/vuln-watch.md](../../docs/bot-runs/vuln-watch.md).
