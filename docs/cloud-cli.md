# Cloud CLI — `iterion remote`

`iterion remote` turns the local CLI into a full client for a remote
iterion instance (cloud or self-hosted `iterion server`): every
operator and admin capability of the HTTP API is reachable as a typed
subcommand. The raw escape hatch `iterion remote api <METHOD> <path>`
remains for anything not wrapped; `iterion remote routes` /
`iterion remote openapi` enumerate the live surface.

All commands honour the global `--json` flag: the output is then the
server's response body verbatim (lossless, stable for `jq`).

The same surface is also exposable to MCP agents (Claude Code, desktop,
Cursor): `iterion mcp` serves `remote_*` tools over the same stored
credential, plus the `remote_api` escape hatch — see
[mcp-server.md](mcp-server.md).

## Setup

### Interactive (browser)

```sh
iterion remote login https://iterion.example.com
```

Opens the instance's `/cli-auth` page; you approve in the studio and a
personal access token (`iap_…`) is stored in `~/.iterion/cli-auth.json`.

Headless alternatives:

```sh
iterion remote login https://… --token iap_…          # existing PAT
iterion remote login https://… --email e@x --password …  # mints a CLI PAT
```

`iterion remote status` shows the logged-in instance + account;
`iterion remote logout` forgets the credential.

### CI / scripting (environment only)

```sh
export ITERION_REMOTE_URL=https://iterion.example.com
export ITERION_REMOTE_TOKEN=iap_…       # fallback: ITERION_TOKEN
iterion remote runs list --json | jq '.runs[].id'
```

When `ITERION_REMOTE_URL` is set the stored config file is ignored
entirely (a stored token is never sent to a different host).
`ITERION_REMOTE_TEAM` / `ITERION_REMOTE_ORG` set the tenant scope.

## Tenant scope (teams / orgs)

Team-scoped commands (secrets, api-keys, webhooks, forge, audit team,
bindings) resolve their team in this order:

1. `--team <id>` flag
2. `ITERION_REMOTE_TEAM`
3. the default persisted by `iterion remote teams switch <id>`
4. the account's active team (from `/api/auth/me`)

A PAT's *identity* team is pinned at mint time, so `teams switch`
mints a **new** token pinned to the target team, stores it, and
revokes the previous CLI token (matched by fingerprint — tokens you
minted for other purposes are never touched). Org-scoped commands
(`--org` / `ITERION_REMOTE_ORG` / `orgs switch`) work the same way but
without re-minting (org scope is path-based).

## The launch → follow → inspect recipe

```sh
# Launch a local .bot file (its source is uploaded inline) and tail it:
iterion remote runs launch ./review.bot --var repo=org/app --follow

# Or launch a catalog bot by id:
iterion remote runs launch --bot whats-next --follow

# Separately:
id=$(iterion remote runs launch ./wf.bot --json | jq -r .run_id)
iterion remote runs follow "$id"          # exit 1 if the run fails
iterion remote runs log "$id"
iterion remote runs artifacts "$id"
iterion remote runs files "$id" src/main.go --diff
iterion remote runs merge "$id" --strategy squash
```

`runs follow` polls `GET /api/runs/{id}/events` by `seq` cursor
(default every 2s, `--interval` to tune) — no WebSocket dependency, so
it works through any proxy.

Attachments: `--attach name=./file` uploads via `POST /api/runs/uploads`
and wires the returned id into the launch. `runs upload <path>` does
the staging step alone and prints the upload id.

## Command tree

| Group | Commands |
|---|---|
| `runs` | `list · launch · get · events · follow · log · workflow · artifacts · files · commits · cancel · pause · resume · fork · send · merge · conflicts · rename · delete · preview-cost · upload · stats · repos` |
| `bots` | `list · get · put · overlay · install · upload` |
| `marketplace` | `list · get · download · submit · install · uninstall · moderation` |
| `issues` | `list · get · create · update · delete · transition · comment · push · pulls` |
| `labels` / `board` | `list · rename · merge · delete` / `get · set` |
| `dispatcher` | `status · state · start · stop · pause · resume · refresh · reload · config · issue · cancel` |
| `triggers` | `list · get · create · update · delete · emit` |
| `schedules` | `list · create · delete` (team-scoped, cloud recurring bots) |
| `teams` | `list · create · switch · members · invitations` |
| `orgs` | `list · switch · members · invitations · usage · teams` |
| `me` | `password · sessions-revoke-all · sso-links` |
| `tokens` | `list · create · revoke` |
| `secrets` / `api-keys` | `list · set/create · rotate/update · delete` (`--scope team\|me`) |
| `bindings` | per-bot secret bindings (`list · create · delete`) |
| `webhooks` | `list · get · create · update · delete · rotate · deliveries` |
| `forge` | `connections · refresh · repo-bots · oauth-apps · integrations` |
| `audit` / `usage` / `limits` | `audit team\|org\|admin` · org usage · cost limits |
| `memory` | `usage · docs · doc get\|put\|delete · export · import` (`--name` space) |
| `admin` | `orgs · users · dlq · llm · caps` (super-admin; `llm api-keys`/`llm oauth` = the platform fallback credentials — rotate without a redeploy, see [cloud-llm-credentials.md](cloud-llm-credentials.md); `caps` = the runtime usage-cap percentages — retune without a restart, see [usage-caps.md](usage-caps.md#changing-the-caps-at-runtime-no-restart)) |
| `sso` | `providers · domains` (org-scoped) |
| `plugins` | `list · enable · disable · install · uninstall · config` |
| `pool` | `status · history · share · pause · resume · withdraw · donors` — lend your own LLM subscription or personal metered key to the shared [credential pool](credential-pool.md), bounded by ceilings you set on `share` (`--max-usd-day/-week`, `--max-runs-day`, `--max-concurrent`, `--from-hour/--to-hour`, `--bots`). `donors` is the operator view of the pool's policy and its lenders. |
| `server` | `info · health` |

Structured mutation payloads follow the `--data '<json>'` /
`--data @file.json` / `--data @-` (stdin) convention shared with
`remote api`.

### Refresh GitHub App grants

After changing a GitHub App installation's permissions on GitHub, refresh the
connection immediately instead of waiting for a periodic worker or restarting
the server:

```sh
iterion remote forge refresh <connection-id>
```

This command applies only to GitHub-App connections. It re-probes the live
installation, replaces the connection's stored grant map, and forces a fresh
installation-token mint. The table deliberately shows **GRANTED** (what the
installation allows) beside **TOKEN** (what the newly minted token actually
carries); `--json` returns both maps plus missing-permission lists. The active
team is resolved by the normal remote-team precedence.

## Secrets hygiene

Secret **values** are never taken as command arguments (argv leaks via
process lists). `secrets set`, `secrets rotate` and `api-keys create`
read the value from `--from-env VAR`, `--from-file path`, or stdin:

```sh
printf '%s' "$GITLAB_TOKEN" | iterion remote secrets set gitlab-token
iterion remote api-keys create --provider anthropic --name prod --from-env ANTHROPIC_API_KEY
```

## Errors

Any non-2xx response surfaces as `HTTP <code> <METHOD> <path>: <first
line of the server's message>` and a non-zero exit. Admin commands do
no client-side role check — a 403 from the server is the answer.

## Everything else

The typed tree covers the operator surface; for the long tail
(examples, filesystem browse, CDP proxies, anything new):

```sh
iterion remote routes                 # live method+path inventory
iterion remote api GET /api/…        # authenticated raw call
```
