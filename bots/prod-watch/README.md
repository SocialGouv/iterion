# prod-watch (Argus)

Production watchdog for one deployed application. Three **deterministic,
zero-LLM** modes: the **watch tick** reads the signals of a running app —
logs through Loki, metrics through Prometheus (both via the Grafana
datasource proxy), Sentry's issues, the app's own health URLs — redacts every raw line
before anything is derived, runs an incident lifecycle every tick, delivers
to chat webhooks and keeps its state in git; the **digest** posts the day in
numbers, always — its absence is the alert; the **session** watches a
declared usage window densely, with immediate escalation, a heartbeat of
positive numbers and a summary. Every message ends with the run that wrote
it.

The whole design (analysis agent reading the code at the deployed ref,
leak policy with a circuit-breaker, Telegram, forge issues, daily digest,
session mode for the active usage windows) lives in the epic
[#1695](https://github.com/SocialGouv/iterion/issues/1695); each slice is a
sub-issue.

## Shape

```
plan → resolve_release → poll_loki → poll_prom → poll_sentry → poll_galerts → probe_http → leak_scan → decide → notify → commit_state → done
plan → watch_halted (typed fail)                          when a halt is armed in the state (watch, session)
plan → done                                               when step_back: a watch tick inside a declared window or
                                                          while a session is open, a session tick outside every window
plan → poll_prom → digest → notify → done                 mode=digest (the ranges, the digest; persists nothing)
decide → session → notify                                 mode=session (start / heartbeat / cap note / summary)
notify → done                                             when not persist (dry-run, partial delivery, the digest)
```

`mode=session` runs the SAME chain on a **fork of the watch state** under
`<state_dir>/session/` — the session is its only writer, the watch state
is never written by a session tick; the first tick of a window copies the
watch state (known incidents are not news, the Loki cursor continues where
watch stopped), and the watch resumes from its own cursor once the window
is over (the catch-up is bounded by `max_window_minutes`, the gap said as
partial coverage — the session saw the window live; an incident the session
escalated and still open is announced once more by the watch). The watch
steps back inside a declared window whatever the envelope, and past the
window only while the envelope is still open (ten minutes of closing-tick
grace plus that tick's run budget); a closed envelope frees it at once. Two
windows back to back continue one fork when the watch state has not changed
between them. Escalation is the
lifecycle's own `new` alert: every new incident posts at once. The
analysis cap (`max_analyses_per_session`) HOLDS the escalations past it —
not posted, not re-fired — and the summary names them.

| node | what it is | reads | writes |
|---|---|---|---|
| `plan` | config + secrets validation, Loki window from the cursor, halt check | `prod-watch.json`, `state.json` | — |
| `resolve_release` | the deployed version from the configured source (`health_field` today); `release_known` stays false until the analysis slice verifies it in the repo | health URL | — |
| `poll_loki` | error query + leak sweep, paged forward over `[from, to)` with `to = now − ingest_lag`; **raw lines go to scratch only** | Grafana proxy | `<scratch>/loki_raw-<run>.jsonl` |
| `poll_prom` | instant queries typed `healthy \| breached \| no_data \| error` | Grafana proxy | — |
| `poll_sentry` | Sentry's org-scoped issue lists: first seen since the cursor, regressed/escalating (dated by their activity), the alerted ones by id; **issue text goes to scratch only** | Sentry API | `<scratch>/sentry_raw-<run>.jsonl` |
| `probe_http` | GET the health URLs, status + latency | the app | — |
| `leak_scan` | **the only reader of the raw lines and the Sentry issue text**: redaction by class, error templates, masked samples, coverage | scratch | `<scratch>/signals-<run>.json` |
| `decide` | incident lifecycle (new / escalated / reminder / quiet, and pending past the cap; for Sentry also regressed / escalating / resolved), source staleness, cap + overflow, staged next state | `signals-<run>.json`, `state.json` | scratch: `state_next-<run>.json`, `alertlog_delta-<run>.jsonl`, `tick-<run>.json` |
| `notify` | Mattermost incoming webhooks (the rendering is Mattermost's), per-sink `min_severity`, `required` sinks, all-or-nothing consume | `webhooks` secret | the channel |
| `commit_state` | `state.json` (replaced), `alertlog.jsonl` + `ticks.jsonl` (appended, `merge=union`), optional commit + push; a session tick writes `session.json` beside them | scratch | `<state_dir>/` (the session's: `<state_dir>/session/`) |
| `session` | mode=session: the start message, a heartbeat every `session_heartbeat_minutes` (config `session.heartbeat.metrics` + every probe + the lanes' counts), the cap note, the summary at the window's end; stages the envelope | `session.json`, the session ledgers | scratch: `session_next-<run>.json` |
| `digest` | mode=digest: ticks executed vs expected (`digest.tick_interval_minutes`), lanes silent, the alerts posted over the window (top error patterns, Sentry and Grafana transitions), the probe success ratio, the Prometheus ranges (`digest.prometheus.ranges`), open incidents, HALTED, release — ALWAYS one message | `ticks*.jsonl`, `alertlog*.jsonl`, `state.json` (read-only) | — |

## Run it

```sh
# in an ops workspace carrying prod-watch.json (see skills/argus-config.md)
iterion run bots/prod-watch/main.bot --var dry_run=true        # first tick: bootstrap + preview, nothing posted
iterion run bots/prod-watch/main.bot                            # a real tick
iterion run bots/prod-watch/main.bot --var state_commit=true    # ephemeral runners: git is the state store
iterion run bots/prod-watch/main.bot --var mode=digest          # the day in numbers, posted whatever it holds
iterion run bots/prod-watch/main.bot --var mode=session --var state_commit=true   # one dense tick of a usage window
```

Secrets: `webhooks` (JSON map name → incoming-webhook URL, shared with
feed-watch and vuln-watch), `grafana_token` (a Grafana service-account
token with datasource query rights), `sentry_token` (a Sentry auth token
with `project:read` + `event:read`), `forge_token` (push of the state dir
on cloud runners). All mounted as read-only files; only deterministic nodes
read them.

That push carries `DELIVERY_OWNED_PATHS` — the state dir and nothing else:
`commit_state` stages and refuses from that one tuple, so a
`.github/workflows/**` path can never be in the diff and the bot owes no
delivery-permission preflight
([why](../../docs/auto-maintenance.md#every-pusher-answers-for-its-delivery)).

## What this slice does NOT do yet

No LLM analysis (the digest is numbers only: its narrative waits for the
analysis slice's gates), no Telegram, no forge issue, no leak policy beyond
"report every class at severity high", no Sentry event body (issue lists
only — the analysis slice reads events). Each is a ticketed slice; the
graph above is the chassis they plug into.
