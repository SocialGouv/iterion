# feed-watch (Vigie) — universal RSS/Atom watch + LLM digest delivered to chat

Vigie replaces a Huginn-style veille scenario (RSS → dedup → digest → LLM →
webhook) with a single bot running in two modes over one file-backed state in
the target workspace. `mode=collect` polls feeds with no LLM at all;
`mode=digest` synthesizes one chat-ready markdown message per category and
POSTs it to Mattermost/Slack incoming webhooks. Nothing workspace-specific is
baked in — categories, feeds, editorial guidance, language, sinks and cadences
all come from a config file in the TARGET repo (default `feed-watch.json`, see
[`skills/feed-config.md`](skills/feed-config.md)) plus the `webhooks` secret.
Requires `python3` (stdlib only) on the execution host. It never edits code.

## When to use it

Run a recurring technology/news watch over RSS/Atom feeds with LLM-synthesized
digests delivered to chat: schedule `mode=collect` runs to poll feeds cheaply,
and per-category `mode=digest` runs (daily/weekly) to synthesize and deliver.
Not for one-shot research questions (use a research bot).

## How it runs

```
plan           (tool)  validate the mode, load the config; config problems
                       fail HERE, before any network or LLM work

mode=collect — zero-LLM, runs with no LLM credential
  fetch_feeds  (tool)  poll every feed (RSS2/Atom/RDF, stdlib parser)
  dedup_queue  (tool)  drop items already in the per-category seen.json FIFO,
                       append the fresh ones to pending.jsonl → done

mode=digest — one LLM step + deterministic delivery
  load_pending (tool)  read the queue + last sent digests, canonicalize urls
                       through redirects; empty queue → done, zero cost
  notify_silence (tool) fires instead when the queue has been empty and
                       nothing delivered for silence_alert_days
  synthesize   (agent) group same-story items, web_fetch the top articles,
                       rank, write ONE digest (permission-gated, WebFetch only)
  verify_message (tool) link firewall — hard-fail if any digest URL is not
                       drawn from the collected items
  notify       (tool)  POST to the configured sinks; never an LLM
  commit_state (tool)  clear exactly the digested items, archive the digest
```

`verify_message` and the `plan` mode/config checks are deterministic gates.
Only a DELIVERED digest reaches `commit_state`: dry-run and sink-less runs keep
every item pending (`notify -> done when not posted`).

## Configuration

| Var | Type | Default | Meaning |
|---|---|---|---|
| `workspace_dir` | string | `${PROJECT_DIR}` | Workspace holding the config + state |
| `mode` | string (enum `collect`, `digest`) | `collect` | Poll feeds, or synthesize and deliver |
| `category` | string | `""` | Config category key; digest REQUIRES it, collect `""` = all |
| `config_path` | string | `feed-watch.json` | Workspace-relative config (`.yaml` needs PyYAML) |
| `state_dir` | string | `.feed-watch` | Workspace-relative state root |
| `dry_run` | bool | `false` | digest: print the would-be payloads, deliver nothing |
| `silence_alert_days` | int | `3` | Days without a delivered digest before warning on the same sinks; `0` disables |
| `state_commit` | bool | `false` | Commit + push the state dir after each mutation (required on ephemeral runners) |
| `fetch_timeout_secs` | int | `20` | Per-feed HTTP timeout at collect |
| `max_items_per_feed` | int | `30` | Freshest-N cap per feed at collect |
| `allow_private_feeds` | bool | `false` | Relax the SSRF guard — trusted single-tenant / on-prem only |
| `max_digest_items` | int | `150` | Newest-N cap passed to the LLM; overflow dropped WITH a count in the message |
| `scratch_dir` | string | `${PROJECT_SCRATCH_DIR}/feed-watch` | Out-of-tree handoff between collect nodes |

The synthesis model is an env, not a var: `FEED_WATCH_MODEL` (also
`FEED_WATCH_BACKEND`, default `claude_code`, and `FEED_WATCH_EFFORT`, default
`medium`).

## Invocation

```bash
# Poll every category's feeds into the pending queues (zero-LLM):
iterion run bots/feed-watch/main.bot --var mode=collect

# Dry-run one category's digest — read the message in the run artifacts:
iterion run bots/feed-watch/main.bot \
  --var mode=digest --var category=cyber --var dry_run=true

# Deliver, versioning the state in git (ephemeral runners):
iterion run bots/feed-watch/main.bot \
  --var mode=digest --var category=cyber --var state_commit=true
```

The manifest declares two `kind: schedule` invocations (`mode: direct`):
collect at `0 5 * * *` and digest at `0 8 * * 1`. Wire them with
`iterion schedule` — one collect for all categories plus one digest schedule
per category; the full recipe (incl. `--tz`) is in
[`skills/feed-config.md`](skills/feed-config.md).

## Notable

- **Secrets** — `webhooks` (JSON map name → incoming-webhook URL) and
  `forge_token`, both mounted `as: file` and `optional: true`, so collect runs
  and dry-run digests need no binding. Sinks reference webhooks by NAME, so
  URLs never enter the repo.
- **`worktree: none`** — the state IS the product and must persist in the
  workspace; a run worktree would discard it at finalization.
- **`permission: deny` + `allow: ["WebFetch(*)", "TodoWrite"]`** — `synthesize`
  takes the category's `editorial` verbatim, so the anti-injection gate hard-
  blocks every non-fetch tool for that one LLM node. Tool nodes are inert under
  the gate. Budget: 1 branch, `30m`, `max_cost_usd: 3`.
- **State** — `<state_dir>/<category>/` holds `seen.json` (dedup FIFO),
  `pending.jsonl` (queue), `digests.jsonl` + `digests/*.md` (sent archive, the
  semantic-dedup context). Concurrent collect/digest serialize on an exclusive
  flock at `<state_dir>/.lock`.
- **Config-share** — the manifest exposes a scoped `config_share` surface so a
  non-operator can edit one category's `feeds` and `editorial` (with
  `digest_title` as read-only context) from a share URL.

## Skills

[`feed-config`](skills/feed-config.md) (config format, state layout, secret,
schedule recipe, SSRF posture), [`synthesis-scoring`](skills/synthesis-scoring.md)
(editorial rubric), [`notify-webhooks`](skills/notify-webhooks.md) (payloads,
dry-run, partial-failure semantics, troubleshooting). Production run log:
[`docs/bot-runs/feed-watch.md`](../../docs/bot-runs/feed-watch.md).
