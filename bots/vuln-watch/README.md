# vuln-watch (Senti) — inventory-scoped vulnerability sentinel, zero LLM

Senti watches published vulnerabilities against the technologies an
organisation actually runs, and posts an actionable alert to chat within the
hour. One deterministic `mode=watch` run over a file-backed state in the
target workspace: poll the security sources, match them against the
workspace's inventory, deliver, advance the state. The compiled workflow has
**no agent and no judge node** — all eight nodes are `tool` nodes running
stdlib `python3` — so a run can neither spend a token nor show a project name
to a model. Nothing workspace-specific is baked in: sources, alert policy,
sinks, message labels and the inventory all come from two JSON files in the
TARGET repo (defaults `vuln-watch.json` + `inventory.json`, see
[`skills/senti-config.md`](skills/senti-config.md)) plus the `webhooks` and
`dependabot_tokens` secrets. It never edits code.

## When to use it

Run an hourly deterministic watch over CVE/GHSA/CERT-style advisories scoped
to a maintained inventory of the technologies and projects you run, with
exploitation-driven noise control. Not an editorial news digest (use
[feed-watch](../feed-watch/README.md)), not a PR dependency gate (use
[supply-shield-cve](../supply-shield-cve/README.md)), not a code audit (use
the `sec-audit-*` bots).

## How it runs

```
plan             (tool)  validate the mode, load + normalize the config and
                         inventory; config problems fail HERE, before any
                         network work
poll_dependabot  (tool)  GitHub org Dependabot alerts, page-walked per org
                         until the per-org cursor
poll_advisories  (tool)  advisory feeds (CERT-FR-style), new publications only
poll_exploit     (tool)  CISA KEV catalog diff
match_policy     (tool)  join everything to the inventory, apply the alert
                         policy (incl. EPSS), stage the next state
confirm_versions (tool)  budgeted version confirmation for the staged alerts
notify           (tool)  POST to the configured sinks; never an LLM
commit_state     (tool)  consume the staged state, append the alert log
```

```
notify -> done          when not consume
notify -> commit_state  when consume
```

Three detection lanes, all structured. The default policy alerts ONLY on an
exploitation signal — a KEV entry, an alert-class advisory, or an EPSS score
at or above the configured threshold. An ordinary new critical stays silent,
is recorded in an observation window, and **re-fires** the day its
exploitation signal lights up. Dedup is deterministic (CVE/GHSA alias sets in
the seen state) and message wording is label-templated, so any language is a
config change. Source failures are explicit: a configured org with no usable
token FAILS the run, and a source silent past `source_stale_hours` is
announced on the sinks.

Delivery-before-persist: only a completed delivery (or a nothing-to-deliver
tick) consumes the staged state. A dry-run, a sink-less run or a failed POST
keeps the old state, so the same alerts recompute next run (at-least-once).

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `${PROJECT_DIR}` | Workspace holding the config, inventory + state |
| `mode` | string (enum `watch`) | `watch` | The only mode (enum-locked) |
| `config_path` | string | `vuln-watch.json` | Workspace-relative source/sink/policy config |
| `inventory_path` | string | `inventory.json` | Workspace-relative technology inventory to match against |
| `state_dir` | string | `.vuln-watch` | Workspace-relative state root |
| `dry_run` | bool | `false` | Compute alerts, deliver nothing, consume nothing |
| `state_commit` | bool | `false` | Commit + push the state dir after each mutation (required on ephemeral runners) |
| `max_version_lookups` | int | `40` | Version-confirmation lookup cap |
| `max_version_seconds` | int | `120` | Version-confirmation wall-clock cap |
| `fetch_timeout_secs` | int | `20` | Per-request HTTP timeout |
| `allow_private_sources` | bool | `false` | Relax the SSRF guard — trusted single-tenant / on-prem only |
| `max_alerts_per_run` | int | `20` | Overflow guard; the rest are reported as a count |
| `observe_window_days` | int | `60` | How long a silent critical stays watched for a re-fire |
| `kev_max_age_days` | int | `90` | Ignore KEV entries older than this |
| `source_stale_hours` | int | `24` | A source silent longer than this is announced stale; `0` disables |
| `scratch_dir` | string | `${PROJECT_SCRATCH_DIR}/vuln-watch` | Out-of-tree handoff between the poll nodes |

The EPSS threshold, the Dependabot alert floor and the CERT-FR `avis` policy
are **config** fields, not vars — they live in `vuln-watch.json` so a policy
change needs no relaunch flags. The launch form renders `mode` and `dry_run`
as primary inputs and hides `scratch_dir`.

## Invocation

```bash
# Hourly watch against a dedicated veille clone:
iterion run bots/vuln-watch/main.bot --var workspace_dir=/path/to/veille-workspace

# Rehearse: compute the alerts, deliver nothing, consume nothing:
iterion run bots/vuln-watch/main.bot --var dry_run=true

# Version the state in git (ephemeral cloud runners):
iterion run bots/vuln-watch/main.bot --var state_commit=true
```

The manifest declares one `kind: schedule` invocation (`mode: direct`,
`context_vars: { mode: watch }`) at a suggested `17 * * * *`. Wire it with
`iterion schedule`; the full recipe is in
[`skills/senti-config.md`](skills/senti-config.md).

## Notable

- **Secrets** — `webhooks` (JSON map name → incoming-webhook URL, identical to
  feed-watch's) and `dependabot_tokens` (JSON map org login → token), plus
  `forge_token` for `state_commit=true` pushes. All three are mounted
  `as: file` and `optional: true`, so a dry-run or a feed/KEV-only deployment
  needs no binding. On an iterion cloud deployment the forge refresh worker
  maintains `dependabot_tokens` from the GitHub App — see
  [docs/forge-security-read.md](../../docs/forge-security-read.md).
- **`worktree: none`** — the state IS the product and must survive the run; a
  run worktree would isolate the writes and discard them at finalization.
  `state_commit=true` therefore expects a DEDICATED workspace clone: it only
  ever stages the state dir, and on a failed rebase drops only its own commit.
- **`repo_devbox: off`** — the bot reads two JSON files and talks to security
  APIs; realising the workspace's toolchain would buy nothing and slow the
  hourly tick.
- **Budget** — 1 branch, `max_duration: 15m`, `max_cost_usd: 0.5` (a belt
  only: there is no LLM node to spend anything).
- **State** — `<state_dir>/` holds `state.json` (seen CVEs/units, per-source
  cursors, health), `alertlog.jsonl` (append-only history of every alert
  posted) and `.lock` (flock serializing state writes).

## Skills

[`senti-config`](skills/senti-config.md) — config format, alert policy,
secrets, inventory format, state layout, schedule recipe, troubleshooting.

## Run history

[`docs/bot-runs/vuln-watch.md`](../../docs/bot-runs/vuln-watch.md).
