---
name: argus-config
description: prod-watch (Argus) workspace configuration — the prod-watch.json format (app, release, grafana, loki, prometheus, probes, sinks, labels), the secrets, the state layout in the ops repository, and the cloud schedule + binding recipe.
---

# prod-watch workspace configuration

prod-watch is fully driven by ONE JSON file in the TARGET workspace — an
**ops repository** dedicated to watching the application, never the
application's own repository (the state is committed there on every tick).
Default path: `prod-watch.json` at the workspace root (`--var config_path=`
overrides).

## Config format (`prod-watch.json`)

```json
{
  "app": {"name": "myapp", "environment": "preprod", "context": "FastAPI API + worker + Vite frontend on Kubernetes; the env named preprod serves users"},
  "release": {"source": "health_field", "health_url": "https://api.myapp.example/api/v1/health", "field": "version"},
  "grafana": {"base_url": "https://grafana.example", "loki_uid": "P8E80F9AEF21F6940", "prometheus_uid": "PBFA97CFB590B2093"},
  "loki": {
    "queries": {
      "errors": "{namespace=\"myapp-preprod\"} |~ \"(?i)error|exception|traceback|critical\"",
      "leak_sweep": "{namespace=\"myapp-preprod\"}"
    },
    "overlap_seconds": 60, "bootstrap_window_minutes": 10, "page_size": 1000,
    "stream_labels": ["namespace", "container", "pod"]
  },
  "prometheus": {
    "probes": [
      {"id": "restarts", "title": "container restarts (30m)", "query": "sum(increase(kube_pod_container_status_restarts_total{namespace=\"myapp-preprod\"}[30m]))", "op": ">", "threshold": 0, "severity": "high"},
      {"id": "oom", "title": "OOMKilled containers", "query": "sum(kube_pod_container_status_last_terminated_reason{namespace=\"myapp-preprod\", reason=\"OOMKilled\"})", "op": ">", "threshold": 0, "severity": "high"},
      {"id": "http_5xx_ratio", "title": "5xx ratio at the ingress (10m)", "query": "sum(rate(nginx_ingress_controller_requests{exported_namespace=\"myapp-preprod\",status=~\"5..\"}[10m])) / clamp_min(sum(rate(nginx_ingress_controller_requests{exported_namespace=\"myapp-preprod\"}[10m])), 1e-9)", "op": ">", "threshold": 0.02, "severity": "critical"}
    ]
  },
  "probes": [
    {"id": "api_health", "url": "https://api.myapp.example/api/v1/health", "expect_status": 200, "timeout_secs": 10, "severity": "critical"},
    {"id": "frontend", "url": "https://myapp.example/live", "expect_status": 200, "severity": "high"}
  ],
  "sinks": [
    {"kind": "mattermost", "webhook": "mm_myapp_ops", "channel": "#myapp-prod", "username": "Argus", "icon_emoji": ":eye:", "min_severity": "medium", "required": true}
  ],
  "labels": {"alert": "alerte production", "reminder": "toujours ouvert"}
}
```

- `app` — `name` (required), `environment` (free text shown in every
  message), `context` (an operator brief; a later slice hands it, fenced,
  to the analysis agent).
- `release` — `source: none | health_field`. With `health_field`, the
  `field` (dotted path) of the JSON body at `health_url` is the deployed
  version. **`release_known` stays false in this slice**: a string found in
  a body is not a verified deployment; the analysis slice resolves it in
  the repository and only then says "known".
- `grafana` — `base_url` (https), the datasource UIDs. The bot calls the
  **datasource proxy** (`/api/datasources/proxy/uid/<uid>/…`) with the
  `grafana_token` secret as a bearer token; the service account needs
  datasource query rights (Viewer is enough on a default Grafana).
- `loki.queries` — a map name → LogQL. Every query feeds the redaction
  scan; every query EXCEPT the one named **`leak_sweep`** also produces
  error templates. Keep `leak_sweep` broad (all lines of the namespace):
  a leak in an `INFO` line is invisible to an error-only query. See
  `skills/signals-and-queries.md` for the window contract.
- `prometheus.probes` — instant queries; `op` ∈ `> >= < <= == !=`;
  `agg` (`max` default, `min`, `sum`, `first`) folds a multi-series
  answer into one value; `severity` ∈ `critical|high|medium|low`. The
  kube-state-metrics and ingress-nginx examples above are **presets, not
  defaults**: check they exist on your cluster (a probe returning no
  series posts a `no_data` incident rather than reading as healthy).
- `probes` — the app's own health URLs. Public hosts only unless
  `--var allow_private_sources=true` (on-prem / hermetic tests).
- `sinks` — same contract as feed-watch/vuln-watch: `webhook` is a NAME
  looked up in the `webhooks` secret; `min_severity` filters what a sink
  receives (notes such as overflow, staleness and partial coverage are
  `medium`); `required: false` marks a best-effort sink whose failure does
  not block the tick.
- `labels` — message-wording overrides (any language). Keys and their
  English defaults live in the bot's `plan` node; placeholders in braces
  are substituted verbatim.

## The secrets

- `webhooks` — JSON map name → incoming-webhook URL, identical to
  feed-watch's. Read only by the deterministic notify step.
- `grafana_token` — a Grafana service-account token. Read only by the two
  proxy lanes; never into a prompt. A configured Grafana with no bound
  token FAILS the run at `plan` (never a zero-signal façade).
- `forge_token` — the ops repository's push credential on cloud runners
  (`state_commit=true`); local runs authenticate through the host.

Cloud: bind team secrets by name (`POST /api/teams/<id>/secrets` with
`{"name":"grafana_token","value":"glsa_…"}`), or a bot-secret binding with
`allowed_hosts` narrowing the egress of that secret to the Grafana host.

## State layout (per workspace)

```
<state_dir>/             default .prod-watch/  (--var state_dir=)
  .lock                  flock serializing state writes (one runner)
  .gitattributes         alertlog.jsonl and ticks.jsonl merge=union
  state.json             cursors + incidents + source health — mode=watch is its ONLY writer
  alertlog.jsonl         append-only history of every alert posted
  ticks.jsonl            append-only tick ledger (the digest slice reads it)
```

Each Loki cursor carries a high-water mark (`covered_to_ns`, never moving
backwards), the frontier where the last walk stopped (`frontier_ns`), the
overlap band of the lines seen there (`band`: `offset:hash` strings from
`band_base_ns`, sha1 prefixes of `timestamp + line`, about 4000 entries at
most — ~150 KiB per query on disk, rewritten every tick as stable lines git
packs well — cut only on a timestamp boundary) and the band's lower bound
(`overlap_from_ns`, which never descends): the next window opens at the
frontier or at the overlap below the mark, never before the band's bound,
so a line is written exactly once across ticks. The hashes are not
reversible, but they are a **confirmation oracle** over raw log content —
one more reason the ops repository that carries the state is private.

Two options, pick ONE: gitignore the state dir (host cron on one
machine), or `--var state_commit=true` (required on ephemeral cloud
runners — git is the state store; the workspace needs push credentials).
**`state_commit=true` expects a DEDICATED ops clone**: the bot declares
`worktree: none` and commits on whatever branch is checked out; it only
ever stages the files it writes in the state dir, by name — a staged path
outside it, or a state file still untracked under an ignore rule, refuses
before a byte of state is written (a tracked state commits whatever the
rules say, as git carries it).

Delivery-before-persist: the state advances only after the webhooks
accepted the messages (or nothing needed posting). A failed delivery
replays the same alerts next tick (at-least-once, never lost);
`dry_run=true` posts nothing and consumes nothing.

**Bootstrap**: the first tick (no `state.json`) reads a short Loki window
(`bootstrap_window_minutes`, default 10) and OBSERVES the error templates
without posting them — an install must not replay history as news — while
failing probes and breached metrics DO post: they describe the present.
An observed template posts as NEW the first time it recurs.

## Scheduling recipe

Host cron:

```sh
W=/path/to/ops-workspace B=bots/prod-watch/main.bot
iterion schedule add prod-watch --cron "*/30 * * * *" --bot $B --workdir $W --var mode=watch --var state_commit=true
iterion schedule install --tz Europe/Paris
```

Cloud (repo-bound schedule on an ephemeral runner):

```sh
iterion remote schedules create --data '{"bot_id":"prod-watch",
  "cron":"*/30 * * * *","repo_url":"https://forge.example/org/myapp-ops.git",
  "repo_ref":"main","vars":{"mode":"watch","state_commit":"true"}}'
```

A scheduled launch mints its clone token from the repo integration when
the ops repo has one; otherwise bind the bot to the forge connection's
managed secret under the name `forge_token` (see vuln-watch's
`senti-config` skill for the binding call — same mechanics).

## Troubleshooting

- **Run failed at `plan`: "config names a Grafana instance but the
  grafana_token secret is not bound"** — bind the secret (cloud: team
  secret named `grafana_token`; local: `iterion secret set grafana_token`).
- **"Grafana refused the token (HTTP 401/403)"** — the service account
  lacks datasource query rights, or the token expired. Nothing was
  consumed; the tick replays.
- **A `no_data` incident on a metric probe** — the query matched no
  series on this cluster: the metric name or labels are wrong for this
  deployment (the ingress/kube-state presets are examples). Fix the
  query; the incident quiets down once the probe answers.
- **`:warning: log coverage this tick was PARTIAL`** — a Loki query was
  truncated at `max_lines` or failed (the failure is quoted under the
  note); the frontier stopped at the last line fetched, so nothing is
  skipped, but absence of a finding proves nothing for that tick. The note
  repeats when the coverage or the kind of failure changes, not every
  tick.
- **The run FAILS with "NO sinks are configured"** — there were alerts and
  nowhere to send them. Deliberate: a schedule reporting success while
  delivering nothing is the silent-green outcome this bot exists to end.
- **Every tick fails the CLONE on cloud** — same cause and fix as
  vuln-watch: bind `forge_token` to the connection's managed secret.
