---
name: feed-config
description: feed-watch (Vigie) workspace configuration — the config file format (categories, feeds, editorial, sinks), the state layout, the webhooks secret, and the iterion-schedule recipe for wiring collect/digest cadences.
---

# feed-watch workspace configuration

feed-watch is fully driven by a config file in the TARGET workspace
(never by anything baked into the bot). Default path: `feed-watch.json`
at the workspace root (`--var config_path=` overrides; `.yaml` works
when PyYAML is installed on the host — JSON needs nothing).

## Config format

```json
{
  "categories": {
    "cyber": {
      "digest_title": "Veille Cyber",
      "feeds": [
        "https://www.cert.ssi.gouv.fr/alerte/feed/",
        "https://krebsonsecurity.com/feed/"
      ],
      "editorial": "Audience: équipe SRE/sécurité francophone. Rédige en français. Priorité absolue: vulnérabilités activement exploitées, alertes CERT-FR, incidents supply-chain. Ignore le marketing produit. Ton: factuel, direct.",
      "sinks": [
        {
          "webhook": "mattermost_dev",
          "channel": "#veille-secu",
          "username": "Vigie Cyber",
          "icon_emoji": ":shield:"
        }
      ]
    }
  }
}
```

Per category:

- `feeds` — RSS 2.0, Atom, or RDF/RSS 1.0 URLs. Parsed with the Python
  stdlib (no feedparser); exotic namespaces are handled by local-name
  matching.
- `editorial` — the category's editorial brief, passed verbatim to the
  synthesis agent: audience, LANGUAGE (the digest is written in the
  language of this text), priorities, exclusions, tone.
- `digest_title` — headline title used in the digest and payloads.
- `sinks` — where the digest goes. `webhook` is a NAME looked up in the
  `webhooks` secret (never a URL — URLs never enter the repo);
  `channel`/`username`/`icon_emoji` are the Mattermost/Slack overrides.
  Several sinks = multi-channel fan-out of the same digest.
  Optional `max_chars` caps THIS sink's messages, overriding the
  `max_message_chars` var (default 14000, sized for Mattermost) — a
  Slack channel alongside a Mattermost one wants a lower number:

  ```json
  { "webhook": "slack_watch", "channel": "#watch", "max_chars": 3500 }
  ```

  A digest above the budget is split into consecutive `_(i/n)_`
  messages per sink, never cut; see `skills/notify-webhooks.md`.

## The `webhooks` secret

A single generic secret named `webhooks` holds the JSON map
name → incoming-webhook URL:

```sh
iterion secret set webhooks '{"mattermost_dev": "https://mm.example.com/hooks/xxx", "mattermost_prod": "https://mm.example.com/hooks/yyy"}'
```

It is mounted as a read-only file (`as: file`) and only read by the
deterministic notify step — never into a prompt. `optional: true`:
collect runs and `--var dry_run=true` digests need no secret at all.
In cloud mode, bind a team secret to the name `webhooks` via a
bot-secret binding.

## State layout (per workspace)

```
<state_dir>/                 default .feed-watch/  (--var state_dir=)
  .lock                      flock serializing concurrent collect/digest
  <category>/
    seen.json                dedup FIFO — ids (cap 5000) + urls (cap 2000)
    pending.jsonl            items awaiting the next digest
    digests.jsonl            sent-digest index (semantic-dedup context)
    digests/*.md             sent digests, human-readable (last 12)
```

Two options for the state dir, pick ONE:

- **gitignored** (default assumption): add `.feed-watch/` to the
  workspace `.gitignore`. Simplest; state lives on the host that runs
  the schedules.
- **versioned** (`--var state_commit=true`): every mutation is
  committed AND pushed (`chore(feed-watch): …`). Required when runs
  execute on ephemeral runners (cloud) — git is the state store. The
  state dir must NOT be gitignored and the workspace needs push
  credentials.

## Scheduling recipe (host cron, no daemon)

One collect for all categories + one digest per category. Example —
daily collect at 05:00, cyber digest daily at 06:00, the rest weekly on
Monday 08:00 (Paris time):

```sh
W=/path/to/veille-workspace B=bots/feed-watch/main.bot
iterion schedule add veille-collect      --cron "0 5 * * *" --bot $B --workdir $W --var mode=collect
iterion schedule add veille-digest-cyber --cron "0 6 * * *" --bot $B --workdir $W --var mode=digest --var category=cyber
iterion schedule add veille-digest-ia    --cron "0 8 * * 1" --bot $B --workdir $W --var mode=digest --var category=ia
iterion schedule install --tz Europe/Paris
```

`--tz` sets `CRON_TZ` for the whole managed block. Collect more often
than you digest (fast feeds scroll): e.g. `0 */12 * * *` for the
collect when a category digests daily. Pair recurring schedules with
retention: `iterion runs prune` (see docs/scheduling.md).

## Run modes & useful vars

| var | default | meaning |
|---|---|---|
| `mode` | `collect` | `collect` or `digest` |
| `category` | `""` | digest: REQUIRED; collect: `""` = all categories |
| `config_path` | `feed-watch.json` | workspace-relative config |
| `state_dir` | `.feed-watch` | workspace-relative state root |
| `dry_run` | `false` | digest: print payloads, deliver nothing |
| `state_commit` | `false` | commit+push state after each mutation |
| `FEED_WATCH_MODEL` (env) | `""` | synthesis model spec (`""` = the resolved backend's default; per-run: `iterion run --model …`) |
| `max_items_per_feed` | `30` | freshest-N cap per feed at collect |
| `max_digest_items` | `150` | newest-N cap per digest (overflow is dropped WITH a count in the message) |
| `fetch_timeout_secs` | `20` | per-feed HTTP timeout at collect |
| `allow_private_feeds` | `false` | relax the SSRF guard (see below) — trusted single-tenant / on-prem only |

First run checklist: create the config, run
`iterion run bots/feed-watch/main.bot --var mode=collect` (zero-LLM —
verify pending.jsonl fills, re-run → 0 new = dedup proven), then
`--var mode=digest --var category=<key> --var dry_run=true` and read
the message in the run artifacts before wiring the real webhooks.

## Feed-fetch security (SSRF posture)

Feed URLs are untrusted input (they come from the workspace config, and
with the config-share editor open, from an operator who is not
necessarily the deployer). The collect step guards every fetch against
SSRF/LFI:

- **Default (`allow_private_feeds: false`) — strict.** Only `http` /
  `https` schemes are fetched, and any host that resolves to a private,
  loopback, link-local, or cloud-metadata address is refused — up-front
  and on every redirect hop — so a hostile feed URL can never reach a
  runner-internal service or read a local file.
- **`allow_private_feeds: true` — relaxed.** Permits private/loopback
  addresses **and** `file://` feeds. Enable it ONLY for a trusted
  single-tenant / on-prem deployment that legitimately polls internal
  feeds. NEVER enable it on a multi-tenant / cloud runner or with the
  config-share editor open — it turns the feed list into an SSRF/LFI
  primitive.

**Sandboxed / cloud runs go through the egress proxy transparently.** A
sandboxed run reaches the internet through iterion's egress proxy
(injected as `HTTPS_PROXY`, advertised at the runner's own private pod
IP — the trusted egress boundary and secret-redaction point, started
even in `network: open` whenever a secret rewriter is present). The
guard detects the proxy from the `*_PROXY` env vars: behind a proxy it
validates each feed URL's host up-front (the real SSRF check on the
untrusted input) rather than at socket-connect time — the socket only
ever targets the proxy, so the proxy itself is exempt. With no proxy
(local / on-prem) the connect-time check stays the guard. Either way an
attacker feed pointing at an internal address is still rejected. No
configuration is needed; feed-watch works the same behind the proxy as
without it.
