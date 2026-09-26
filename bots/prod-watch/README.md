# prod-watch (Argus)

Production watchdog for one deployed application. This slice is the
**deterministic, zero-LLM watch tick**: it reads the signals of a running
app — logs through Loki, metrics through Prometheus (both via the Grafana
datasource proxy), the app's own health URLs — redacts every raw line
before anything is derived, runs an incident lifecycle every tick, delivers
to chat webhooks and keeps its state in git.

The whole design (analysis agent reading the code at the deployed ref,
leak policy with a circuit-breaker, Telegram, forge issues, daily digest,
session mode for the active usage windows) lives in the epic
[#1695](https://github.com/SocialGouv/iterion/issues/1695); each slice is a
sub-issue.

## Shape

```
plan → resolve_release → poll_loki → poll_prom → probe_http → leak_scan → decide → notify → commit_state → done
plan → watch_halted (typed fail)                          when a halt is armed in the state
notify → done                                             when not consume (dry-run, partial delivery)
```

| node | what it is | reads | writes |
|---|---|---|---|
| `plan` | config + secrets validation, Loki window from the cursor, halt check | `prod-watch.json`, `state.json` | — |
| `resolve_release` | the deployed version from the configured source (`health_field` today); `release_known` stays false until the analysis slice verifies it in the repo | health URL | — |
| `poll_loki` | error query + leak sweep, paged forward over `[from, to)` with `to = now − ingest_lag`; **raw lines go to scratch only** | Grafana proxy | `<scratch>/loki_raw-<run>.jsonl` |
| `poll_prom` | instant queries typed `healthy \| breached \| no_data \| error` | Grafana proxy | — |
| `probe_http` | GET the health URLs, status + latency | the app | — |
| `leak_scan` | **the only reader of the raw lines**: redaction by class, error templates, masked samples, coverage | scratch | `<scratch>/signals-<run>.json` |
| `decide` | incident lifecycle (new / escalated / reminder / quiet), source staleness, cap + overflow, staged next state | `signals-<run>.json`, `state.json` | scratch: `state_next-<run>.json`, `alertlog_delta-<run>.jsonl`, `tick-<run>.json` |
| `notify` | Mattermost/Slack incoming webhooks, per-sink `min_severity`, `required` sinks, all-or-nothing consume | `webhooks` secret | the channel |
| `commit_state` | `state.json` (replaced), `alertlog.jsonl` + `ticks.jsonl` (appended, `merge=union`), optional commit + push | scratch | `<state_dir>/` |

## Run it

```sh
# in an ops workspace carrying prod-watch.json (see skills/argus-config.md)
iterion run bots/prod-watch/main.bot --var dry_run=true        # first tick: bootstrap + preview, nothing posted
iterion run bots/prod-watch/main.bot                            # a real tick
iterion run bots/prod-watch/main.bot --var state_commit=true    # ephemeral runners: git is the state store
```

Secrets: `webhooks` (JSON map name → incoming-webhook URL, shared with
feed-watch and vuln-watch), `grafana_token` (a Grafana service-account
token with datasource query rights), `forge_token` (push of the state dir
on cloud runners). All mounted as read-only files; only deterministic nodes
read them.

## What this slice does NOT do yet

No LLM analysis, no Telegram, no forge issue, no leak policy beyond
"report every class at severity high", no digest, no session mode. Each
is a ticketed slice; the graph above is the chassis they plug into.
