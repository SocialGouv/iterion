---
name: senti-config
description: vuln-watch (Senti) workspace configuration — the config file format (sources, alert policy, sinks, labels), the technology inventory format, the two secrets, the state layout, and the schedule recipe for the hourly watch.
---

# vuln-watch workspace configuration

vuln-watch is fully driven by two JSON files in the TARGET workspace
(never by anything baked into the bot): the config (default
`vuln-watch.json`, `--var config_path=` overrides) and the technology
inventory (default `inventory.json`, or the config's `inventory_path`).

## Config format (`vuln-watch.json`)

```json
{
  "github_orgs": ["my-org", "my-other-org"],
  "advisory_feeds": [
    {"url": "https://www.cert.ssi.gouv.fr/alerte/feed/", "kind": "alert"},
    {"url": "https://www.cert.ssi.gouv.fr/avis/feed/", "kind": "advisory"}
  ],
  "epss_threshold": 0.5,
  "dependabot_alert_floor": "exploited",
  "certfr_avis": "observe",
  "inventory_path": "inventory.json",
  "sinks": [
    {"webhook": "mattermost_prod", "channel": "#security-alerts",
     "username": "Senti", "icon_emoji": ":shield:"}
  ],
  "labels": {"projects": "Projets concernés", "fix": "Correctif"}
}
```

- `github_orgs` — org logins whose **Dependabot alerts** are polled
  (lane A). Each needs a token in the `dependabot_tokens` secret.
- `advisory_feeds` — RSS/Atom advisory feeds (lane B). `kind: "alert"`
  marks a feed whose publications are BY THEMSELVES an exploitation
  signal (CERT-FR *alerte*); `kind: "advisory"` (or a bare URL string)
  is observe-by-default. When a feed item's `<link>/json/` answers with
  a CERT-FR-shaped document (`cves[]`, `affected_systems[]`), matching
  uses that structured data; otherwise title+summary text.
- `kev_url` / `epss_url` — default to the public CISA KEV catalog and
  FIRST EPSS API (lane C). Set `""` to disable a lane, or point them at
  a mirror.
- `epss_threshold` — EPSS probability at/above which a matched CVE is
  considered exploited (default 0.5). `null` with `epss_url: ""`
  disables the signal.
- `dependabot_alert_floor` — when a NEW Dependabot advisory may alert
  without an exploitation signal: `exploited` (default — never),
  `critical`, or `high`.
- `certfr_avis` — `observe` (default) parks matched non-alert
  advisories in the observation window; `alert` posts them.
- `sinks` — same contract as feed-watch: `webhook` is a NAME looked up
  in the `webhooks` secret (URLs never enter the repo);
  `channel`/`username`/`icon_emoji` are the Mattermost/Slack overrides.
- `labels` — message-wording overrides (any language). Keys and their
  English defaults live in the bot's `plan` node; override any subset.
  Placeholders in braces (`{n}`, `{date}`, `{signal}`, `{source}`,
  `{hours}`, `{last}`, `{pct}`, `{sev}`) are substituted verbatim.
- `github_api_base` — override for GitHub Enterprise (default
  `https://api.github.com`).

## The alert policy (why your critical did not post)

A matched vulnerability posts immediately ONLY on an exploitation
signal:

- its CVE is in the **CISA KEV** catalog (or a NEW KEV entry matches an
  inventory technology directly),
- it arrived through an **alert-class feed** (`kind: "alert"`),
- its **EPSS** score ≥ `epss_threshold`.

Everything else — ordinary new criticals included — is recorded in a
60-day observation window (`--var observe_window_days=`) and re-scored
every run: the day its signal lights up, it fires ONCE, with a note
saying when it was first observed. That re-fire is the core scenario:
a CVE lands quietly, mass exploitation starts two weeks later, the
alert goes out that hour — not the day the CVE was assigned.

Dedup is by CVE alias set (GHSA↔CVE from the GitHub advisory,
`cves[]` from CERT-FR JSON): the same vulnerability arriving through
two lanes posts once. New repos joining an already-alerted advisory
stay silent (counted in the state).

## The two secrets

- `webhooks` — JSON map name → incoming-webhook URL, identical to
  feed-watch's. Read only by the deterministic notify step.
- `dependabot_tokens` — JSON map `{org_login: token}` (lowercase org
  logins; `"*"` = fallback token for every org). Two ways to fill it:
  - **iterion cloud + GitHub App**: enable security-read on the
    connection (`PATCH /api/teams/{id}/forge/connections/{conn_id}`
    with `{"security_read_enabled": true}`) — the forge refresh worker
    then maintains this secret automatically from short-lived
    installation tokens. See docs/forge-security-read.md.
  - **anywhere else**: fine-grained PATs with "Dependabot alerts:
    read-only" (one per org), set by hand:
    `iterion secret set dependabot_tokens '{"my-org": "github_pat_..."}'`.

  A configured org with no usable token FAILS the run explicitly —
  never a silent zero-alert.

## Inventory format (`inventory.json`)

```json
{
  "generated": {"carto": "2026-06-11", "github": "2026-08-24"},
  "technologies": {
    "metabase": {
      "label": "Metabase",
      "category": "analytics",
      "match": ["metabase"],
      "watch": true,
      "projects": ["domifa"]
    },
    "jwt": {"label": "JWT", "match": [], "watch": false, "projects": []}
  },
  "projects": {
    "domifa": {"name": "DOMIFA", "repos": ["my-org/domifa"], "channel": null}
  }
}
```

- `technologies.<key>.match` — lowercase keywords matched
  **word-boundary** against advisory titles/product lists and KEV
  vendor/product fields. Multi-word phrases allowed ("spring boot").
  Empty list or `watch: false` = inventoried but never matched (keep
  concept-level entries like SQL/JWT from producing noise).
- `technologies.<key>.projects` — project keys this techno serves;
  they MUST exist under `projects` (plan validates).
- `projects.<key>.repos` — `org/repo` full names; lane A joins
  Dependabot repos back to projects through them.
- `projects.<key>.channel` — reserved for future per-project routing
  (null today).

Generate and refresh it with whatever analysis fits the org (the
SocialGouv deployment generates it from the carto reports + a GitHub
org sweep — scripts live in the veille workspace repo, not in the
bot). The file is committed to the workspace repo, so project names
live in that repo's privacy scope and NEVER pass through an LLM (the
workflow has no LLM node — that is a tested invariant).

## State layout (per workspace)

```
<state_dir>/            default .vuln-watch/  (--var state_dir=)
  .lock                 flock serializing state writes
  state.json            seen CVEs/units + per-source cursors + health
  alertlog.jsonl        append-only history of every alert posted
```

Same two options as feed-watch, pick ONE: gitignore the state dir, or
`--var state_commit=true` (required on ephemeral cloud runners — git
is the state store; the workspace then needs push credentials, e.g.
the `forge_token` binding).

Delivery-before-persist: the state only advances after the webhooks
accepted the messages (or nothing needed posting). A failed delivery
replays the same alerts next run (at-least-once, never lost).
`dry_run=true` posts nothing and consumes nothing.

**Bootstrap**: the first run (no state.json) sets every cursor to
"now" and alerts NOTHING — the pre-existing backlog is not news.
Verify the wiring with a dry-run after seeding state, not on the
bootstrap tick.

## Scheduling recipe

Hourly watch (the "within the hour" promise), host cron:

```sh
W=/path/to/veille-workspace B=bots/vuln-watch/main.bot
iterion schedule add vuln-watch --cron "17 * * * *" --bot $B --workdir $W \
  --var mode=watch --var state_commit=true
iterion schedule install --tz Europe/Paris
```

Cloud (repo-bound schedule on an ephemeral runner):

```sh
iterion remote schedules create --data '{"bot_id":"vuln-watch",
  "cron":"17 * * * *","repo_url":"https://github.com/my-org/veille.git",
  "repo_ref":"main","vars":{"mode":"watch","state_commit":"true"}}'
```

## Troubleshooting

- **Run failed: "no Dependabot token for configured org(s)"** — the
  `dependabot_tokens` secret is missing/incomplete. App path: check the
  connection's health (`missing_security_permissions`) and its
  `security_read_enabled` flag. PAT path: re-set the secret.
- **Run failed: "GitHub rejected the Dependabot token"** — expired or
  under-scoped token; the App refresh worker logs a warn per cycle when
  its mint fails (grant revoked?).
- **`:warning: source silent` message** — a source has had no
  successful poll for `source_stale_hours` (default 24h). The run log
  (`match_policy` summary) carries the per-source errors.
- **Nothing posts, ever** — check the run summary: `alerts: 0 to post,
  N observed` is the policy working (nothing exploited); `BOOTSTRAP`
  means the first tick just armed the cursors; `suppressed` counts
  dedup hits.
- **Same alert twice** — expected only if delivery partially failed
  (state not consumed) or the tier escalated (one re-fire per unit,
  by design).
