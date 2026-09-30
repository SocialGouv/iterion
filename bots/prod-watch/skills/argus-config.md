---
name: argus-config
description: prod-watch (Argus) workspace configuration — the prod-watch.json format (app, release, grafana, loki, prometheus, probes, sentry, sinks, labels), the secrets, the state layout in the ops repository, and the cloud schedule + binding recipe.
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
  "sentry": {"base_url": "https://sentry.example", "org": "myorg", "project": "myapp", "environment": "preprod", "min_level": "error"},
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
  version — one token (letters, digits, `._+-`, 120 at most; a build
  suffix like `1.2.3+build.4` passes, an email address or a URL cannot);
  any other text is withheld and noted. **`release_known` stays false in this slice**: a string found in
  a body is not a verified deployment; the analysis slice resolves it in
  the repository and only then says "known".
- `grafana` — `base_url` (https), the datasource UIDs. The bot calls the
  **datasource proxy** (`/api/datasources/proxy/uid/<uid>/…`) with the
  `grafana_token` secret as a bearer token; the service account needs
  datasource query rights (Viewer is enough on a default Grafana).
  `deadline_secs` (90, 10–600): each Grafana lane — Loki, Prometheus —
  starts no request after running that long, and the one running then
  stops there; the queries or probes left are the lane's errors.
- `loki.queries` — a map name → LogQL. Every query feeds the redaction
  scan; every query EXCEPT the one named **`leak_sweep`** also produces
  error templates. Keep `leak_sweep` broad (all lines of the namespace):
  a leak in an `INFO` line is invisible to an error-only query. Size it
  to the namespace's rate: `max_lines` new lines a tick (5000 by default)
  at the schedule's cadence is the sweep's throughput (about 2.8 lines/s
  at `*/30`); above it the sweep runs behind, every tick is partial
  coverage and the cursor eventually declares a gap — raise `max_lines`
  or the cadence. See `skills/signals-and-queries.md` for the window
  contract.
- `prometheus.probes` — instant queries; `op` ∈ `> >= < <= == !=`;
  `agg` (`max` default, `min`, `sum`, `first`) folds a multi-series
  answer into one value; `severity` ∈ `critical|high|medium|low`. The
  kube-state-metrics and ingress-nginx examples above are **presets, not
  defaults**: check they exist on your cluster (a probe returning no
  series posts a `no_data` incident rather than reading as healthy).
- `probes` — the app's own health URLs. Public hosts only unless
  `--var allow_private_sources=true` (on-prem / hermetic tests). A probe's
  `timeout_secs` (10) bounds it end to end, redirects included: an app
  trickling its answer fails the probe as a timeout. Every other fetch is
  bounded by `--var fetch_timeout_secs` (20): the release endpoint end to
  end, a Grafana call — its one retry included — within six of them (a
  large page arriving slowly is legitimate; a stall trips the socket
  timeout first) and never past its lane's `grafana.deadline_secs`.
  **The run's budget** (12 minutes) must hold every wait before the
  delivery at its worst: the release endpoint and its host's lookup (2 ×
  `fetch_timeout_secs`), each probe tried twice two seconds apart plus
  one lookup per probed host, each lane's deadline (a name lookup is
  bounded like the answer after it). `plan` refuses a config whose sum
  passes 480 s (a tick the budget kills posts nothing, the health probes
  included), naming each wait. The delivery has until the time the
  budget keeps for it — at least a minute —, the required sinks first, a
  sink that timed out once not asked again that tick (20 s a post); the
  state commit has its own window (90 s, 60 s a git call).
- `sentry` — absent or `null`: the lane is off. `base_url` (https, no
  query, fragment or credentials; the prefix of the ONE clickable link
  the bot renders), `org` and `project` (slugs), `environment` (strongly
  recommended: with it, NEW means "first event processed in this
  environment", a server-side processing time; without it Sentry filters
  on the issue's first event time, which a late event or a client clock
  can move; `none` is refused — Sentry reads it as two different
  environments), `min_level` (`fatal|error|warning|info|debug`, default
  `error`: below it the lane never STARTS tracking an issue — an issue
  already alerted stays observed whatever its latest event's level),
  `severity` (level → `critical|high|medium|low`, defaults fatal→high,
  error→medium, warning and below→low), `max_severity` (default `high`:
  a level is event content anyone with the public DSN writes, so
  `critical` from Sentry is an opt-in; lowering it caps the incidents the
  lane already knows too), `overlap_minutes` (60),
  `max_issues` (per list, pages of 100; default 200),
  `max_transition_checks` (activity lookups per tick; 20),
  `max_tracked` (issues re-read by id each tick; 200 — the open ones
  first, then the resolved ones, each taking turns least recently read
  first, an issue read in a list counting as read; a coverage note says
  the cut once the open ones pass it; 0 turns the by-id reads off), `deadline_secs` (120, a wall clock: the walk stops there, partial,
  never the tick's death), `max_catchup_hours` (24, above
  `overlap_minutes`: a cursor older than that — the lane turned off, a
  long outage — opens at the floor and declares the gap instead of
  posting days-old issues as new, and a transition dated before the floor
  is history). The lane's IDENTITY — base URL (compared as the host it
  names: case and a default port do not count), org, project,
  environment — travels in its cursor: changing any of them
  drops the old identity's incidents and cursor, and re-arms the lane (a
  silent bootstrap). `min_level` is not part of it: moving it drops
  nothing. See `skills/signals-and-queries.md` for what posts when.
- `sinks` — same contract as feed-watch/vuln-watch: `webhook` is a NAME
  looked up in the `webhooks` secret; `min_severity` filters what a sink
  receives (notes such as overflow, staleness and partial coverage are
  `medium`, and bypass `min_severity`: a sink that only pages for critical
  still learns the bot went blind); a message that reached NO sink fails
  the tick and replays whatever `required` says — `required: false` marks
  a best-effort sink whose own failure alone does not fail the tick.
- `labels` — message-wording overrides (any language). Keys and their
  English defaults live in the bot's `plan` node; placeholders in braces
  are substituted with each value as inline code (neither a mention nor
  a link can come out of it), truncated to 200 characters; the label's
  own words render as written — its markdown, links and LaTeX are shown,
  never live (a label cannot format, link or tag): it is one line, every
  dot is escaped, an `@` before a word mentions nobody, an emoji code is
  text unless a space follows it (its colons could open a scheme),
  parentheses and plain colons stay as they are. `sentry_reopened` says a
  closed issue is open again; `folded_detail` words the note naming, per
  kind, the alerts a minting lane has past `--var max_alerts_per_lane` (5) —
  `folded_detail_more` the same note when it holds some back for the
  next ticks (`{more}`) — their header is the kind's own; both hold
  `{names}` exactly once, in 400 characters at most (plan refuses an
  override otherwise: a note says its members by name, within the
  message).

## The secrets

- `webhooks` — JSON map name → incoming-webhook URL, identical to
  feed-watch's. Read only by the deterministic notify step. Each URL must
  be one http(s) URL with a host, no credentials (`user:password@`) and a
  port that parses; a malformed one fails that sink's delivery, named by
  the webhook — a URL is never quoted (its path is the key).
- `grafana_token` — a Grafana service-account token. Read only by the two
  proxy lanes; never into a prompt; one token on one line (whitespace or
  a control character inside is refused, never quoted; a byte-order mark an
  editor saved is dropped). A configured
  Grafana whose token is unbound, blank, malformed or refused is a lane
  error on every query and probe,
  named in the coverage note: the health probes still report (with no
  other lane configured, the tick is refused, naming it).
- `sentry_token` — a Sentry auth token with `project:read` and
  `event:read` on the watched project (an internal integration's, or a
  user token — nothing more: the lane never writes). Read only by
  `poll_sentry`; one token on one line, never quoted. Unbound, blank,
  malformed or refused (401/403) is a lane error named in the coverage
  note; the other lanes still report.
- `forge_token` — the ops repository's push credential on cloud runners
  (`state_commit=true`); local runs authenticate through the host.

Cloud: bind team secrets by name (`POST /api/teams/<id>/secrets` with
`{"name":"grafana_token","value":"glsa_…"}`), or a bot-secret binding with
`allowed_hosts` narrowing the egress of that secret to the Grafana host.

## State layout (per workspace)

```
<state_dir>/             default .prod-watch/  (--var state_dir=)
  .lock                  flock serializing state writes (one runner)
  .gitignore             keeps .lock out of git (written by the bot)
  .gitattributes         alertlog.jsonl and ticks.jsonl merge=union
  state.json             cursors (Loki, Sentry) + incidents + source health — mode=watch is its ONLY writer
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

The Sentry cursor is `{identity, armed_at, since, at}`; a Sentry incident
persists structured fields only (short id, level, link, the last event
time, the transition date, backlog/pending flags) — issue titles and
culprits are rendered from each tick's scrubbed signals and never written
to the state or the alert log, which are committed for good.

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
An observed template posts as NEW the first time it recurs. The Sentry
lane's first tick records every issue it reads (the issues first seen in
the overlap, the regressed and escalating ones) as backlog and posts
nothing; unlike a log template, a backlog issue never posts on mere
recurrence — only through Sentry's own transitions (a regression, an
escalation) dated after the arming.

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

- **"CredentialRefused: the grafana_token secret is not bound or empty"**
  in a coverage note — bind the secret (cloud: team secret named
  `grafana_token`; local: `iterion secret set grafana_token`).
- **"CredentialRefused: Grafana refused the token (HTTP 401/403)"** in a
  coverage note — the service account lacks datasource query rights, or
  the token expired. **"OSError: Grafana host '…'"** — the Grafana host
  does not resolve (or resolves to an address the guard refuses). In each
  case the Loki and Prometheus lanes fail and the health probes still
  report. The Loki cursors do not move meanwhile: fix it within
  `max_window_minutes` minus `overlap_seconds` of the last good tick and
  nothing is skipped; past that, the window opens at its floor and the
  stretch below it is a declared gap. With no health probe configured,
  the run fails instead ("every configured lane failed"), naming it.
- **A `no_data` incident on a metric probe** — the query matched no
  series on this cluster: the metric name or labels are wrong for this
  deployment (the ingress/kube-state presets are examples). Fix the
  query; the incident quiets down once the probe answers.
- **`:warning: coverage this tick was PARTIAL (reasons below)`** — a
  Loki query was truncated at `max_lines`, fell out of the max window (a
  gap) or failed, a Prometheus probe failed, or the template list was
  cut at 200; the reasons are quoted under the note, a gap first. A
  truncated or failed walk skips nothing (the frontier stopped at the
  last line read, the next tick reads on), but absence of a finding
  proves nothing for that tick. A **gap does skip lines**: the cursor
  fell out of the max window and the lines in between are never read —
  raise `max_lines` or the cadence (or `max_window_minutes`). Each
  component of the partiality (a query's gap, a lane's error kind, a
  truncation, a cut) is said once, then again only after
  `renotify_hours`, whatever the combination: queries or probes failing
  in turn with the same error, or a lane flapping, do not re-post it
  every tick.
- **"CredentialRefused: Sentry refused the token (HTTP 401/403)"** —
  the `sentry_token` lacks `project:read`/`event:read` on the project, or
  it expired. **"environment 'X' is unknown to project org/proj"** — a
  typo, or an environment no event of that project ever carried (Sentry
  creates it with its first event). **"project org/proj not found, or not
  visible to the token"** — the slug, or the token's scope. Each is a lane
  error: the other lanes still report.
- **`sentry: partial walk (…)`** in a coverage note — a list stopped at
  `max_issues`, the activity lookups at `max_transition_checks`, the
  deadline passed, or an issue was malformed; the causes are named
  (open issues over `max_tracked` have their own note, **`sentry: N tracked issues for max_tracked M
  reads a tick`**: they take turns — raise it; a deleted issue keeps its turn until retention forgets
  it). Nothing is concluded from absence that
  tick (no "not observed any more"); a flood of distinct issues from a
  public DSN is one way there — raise the caps or tighten `min_level`
  (past `max_alerts_per_lane`, the flood's issues are named in notes of
  their kind the tick they come, and their follow-ups fold too — set a
  rate limit on the DSN key in Sentry to stop it at the source). **`sentry: … carried no
  Link header`** — a proxy between the runner and Sentry strips it: no
  list can be read whole, so the lane never arms (or its cursor stays)
  until the proxy passes it. **`sentry: gap — the cursor was older
  than max_catchup_hours …`** — issues first processed in the named
  interval were never read as new (a loss, said once). **`sentry: NOT
  ARMED — …`** — the bootstrap could not read the new-issue list whole
  (more issues first seen in the overlap than `max_issues`, an error, the
  deadline): each tick bootstraps again until it can, and nothing posts
  from the lane meanwhile — its health is not stamped either, so a
  silent-source note follows. Raise `max_issues` or `deadline_secs`, or
  shorten `overlap_minutes`. **`sentry: P-… regressed or escalated at …
  after the arming but was dated only past max_catchup_hours — recorded as
  history, not posted`** — a transition the lane first saw too late to call
  news (it was off, the level floor was lowered, or the issue was new to
  it): never posted, and named until a note has said it (after the lane's
  losses: the note's budget can defer it to a later tick). **`sentry: N
  more transition(s) recorded as history …`** — past 100 names waiting,
  the oldest are counted instead.
- **The run FAILS with "the fetches can wait N s at worst (…)"** — the
  release endpoint, the probes (their retry and lookup counted) and the
  lanes' deadlines add up
  past what the 12-minute budget keeps before the delivery: lower
  `grafana.deadline_secs`, `sentry.deadline_secs`, a probe's
  `timeout_secs` or `fetch_timeout_secs` as the message names them.
- **The run FAILS with "NO sinks are configured"** — there were alerts and
  nowhere to send them. Deliberate: a schedule reporting success while
  delivering nothing is the silent-green outcome this bot exists to end.
- **Every tick fails the CLONE on cloud** — same cause and fix as
  vuln-watch: bind `forge_token` to the connection's managed secret.
