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
iterion remote login https://… --email e@x --password … --team <id|slug>  # pinned to a team
```

A `--token` (or `ITERION_TOKEN`) login is saved only when the instance
answers `GET /api/auth/me` with that token's account — a JSON identity with
a user id. A redirect is followed nowhere (the token is never sent to its
target) — and when the target is another host the refusal names it as the
URL to log in to instead — and a redirect, an HTML page, an unrelated JSON
body or an error status refuses the login with nothing saved; a server
error (5xx) says to retry once the server answers. `--team` pins the token
a `--email` login mints — a team UUID or a team slug, resolved against your
own org tree under the login's session, like `teams switch`, and saved as
the team's UUID; it is refused with `--token` (a token's team is fixed at
its mint) and with the browser flow (the approval pins the studio's active
team).

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
bindings, and the run reads — `runs list`, `runs stats`, `runs repos`,
`resume --usage-blocked`) resolve their team in this order:

1. `--team <id|slug>` flag (a slug resolves against the org tree, an
   unknown one is refused naming it)
2. `ITERION_REMOTE_TEAM`
3. the default persisted by a `--team` login (or by `teams switch`)
4. the account's active team (from `/api/auth/me`)

A PAT's *identity* team is pinned at mint time, and a PAT may not mint
another credential (the instance answers `403`). So `teams switch` re-opens
the browser approval (the studio's `/cli-auth` page) with the team pinned:
the approving live session mints the team-pinned token — never the stored
PAT — and the CLI stores it and revokes the previous CLI token (matched by
fingerprint — tokens you minted for other purposes are never touched).
Headless, pin the team at login instead:
`iterion remote login <url> --email <you> --team <id>`. From a session
credential the switch still mints directly, stores, and revokes the same
way. `iterion remote tokens create` is refused the same way
from a stored token: create the token in the studio (Account → Access
tokens) or from a fresh login.

The same rule reaches every command whose route mints a **team token** — a
token that outlives the credential it is minted from. Three of them mint one
on every call, so the CLI states the refusal itself, before anything is sent
— not even the `/api/auth/me` the team and org scope resolution would read:
`webhooks create`, `webhooks rotate` and `teams|orgs invitations create`.
The advice names the **studio**, not a login: a login stores another personal
access token, which mints no more than this one. Reads and the gestures that
mint nothing stay available — `webhooks list`, the invitation list and
`DELETE`, `repo-bots preview|delete`, `orgs approvals reject`. Org-scoped
commands (`--org` / `ITERION_REMOTE_ORG` / `orgs switch`) need no re-minting
(org scope is path-based).

The repo-bot provisioning is the one the CLI does **not** pre-judge, and must
not: whether it mints depends on state only the instance holds. A repo's
FIRST enable reserves its hook identity and mints the `iwh_` token, so it is
refused (403) from a stored token. A **reconcile** of a repo already
provisioned, a settings-only `repo-bots update`, and an `orgs approvals
approve` standing for such an update reuse the integration's sealed secret,
mint nothing, and go through — which is why `forge repo-bots create|update`,
`orgs approvals approve` and the same calls through `remote api` all send,
and the instance decides. The question is asked where the mint is decided, so
a refusal that does come back carries the same studio advice.

## The launch → follow → inspect recipe

```sh
# Launch a local .bot file (its source is uploaded inline) and tail it.
# What travels is the bot written out as ONE file: a bot in several files
# (`import "lib/x.bot"`) is flattened, and every prompt `{{include "x.md"}}`
# is resolved beside the file that carries it, before the upload — a server
# pod has none of the files beside the source. A bot in one file with no
# include uploads byte-identical. The run's identity is that text: a resume
# (`runs resume --file`) re-flattens the same unit into the same text, an
# edited fragment or include into another, which the server refuses without
# --force.
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

What a server relays — a run's error, a title, a body without a table —
is shown as text: a control character, an escape sequence or a bidi
override is printed escaped (`\x1b`, `\u202e`), never sent to the
terminal raw. `--json` passes the server's body as it is, for a machine;
`runs log` and `runs workflow` print the bytes the run holds, as they are
(a log keeps its colors). A long run error is clipped on its failure's
text: the remedy it ends on (`— hint: …`) is kept whole.

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
| `labels` / `board` | `list · rename · merge · delete` / `get · set · bind · show · unbind` |
| `dispatcher` | `status · state · start · stop · pause · resume · refresh · reload · config · issue · cancel` |
| `triggers` | `list · get · create · update · delete · emit` |
| `schedules` | `list · create · delete` (team-scoped, cloud recurring bots) |
| `teams` | `list · create · switch · members · invitations · update · status · delete · add-member` |
| `orgs` | `list · switch · members · invitations · usage · teams · add-member · approvals · credential-audience · oauth · settings` |
| `… members` (both) | `members` alone lists; `add <user-id> <role>` places an account that ALREADY exists (idempotent), `set-role` updates an existing membership, `remove` revokes. A team grant needs the user in the team's org first, so the order for someone new is `orgs members add` then `teams members add`. **`orgs members add` is super-admin only** — an org admin adds by `orgs invitations create <email>` (from the studio, or from a CLI holding a session credential: the invitation is a team token, which a stored personal access token does not mint) and keeps `set-role`/`remove`; `teams members add` stays theirs, its candidates being already in the org. |
| `me` | `password · sessions-revoke-all · sso-links` |
| `tokens` | `list · create · revoke` |
| `secrets` / `api-keys` | `list · set/create · rotate/update · delete` (`--scope team\|me`) |
| `bindings` | per-bot secret bindings (`list · create · delete`) |
| `price-feeds` | `list · set · delete · sync [feed]` (`--team \| --org`) — the gateway price feed (ADR-163): auto-synced operator pricing in the database, fresh entries answering over the operator table, `sync` reporting what changed per id. Sources: `modelsdev · scaleway · litellm · exec` — `exec` is super-admin only and runs only where the deployment exports `ITERION_PRICE_FEED_EXEC=1`; `interval`/`timeout` in seconds. See [backends.md](backends.md#the-gateway-price-feed-auto-synced-operator-pricing-adr-163). |
| `webhooks` | `list · get · create · update · delete · rotate · deliveries` (`create` and `rotate` mint a team token on every call: studio only from a stored token) |
| `forge` | `connections · refresh · repo-bots · oauth-apps · integrations` |
| `audit` / `usage` / `limits` | `audit team\|org\|admin` · org usage · cost limits |
| `memory` | `docs · doc get\|put\|delete · export · import` (`--name` space) — `usage` carries no space flags, so the server refuses it |
| `admin users` | `users` alone lists (`--q` matches an email PREFIX or an exact id); `get <user-id>` returns one account's file — status, last sign-in, whether a password sign-in is possible at all, its SSO links, and the orgs/teams it was actually granted; `update <user-id> --data`; `reset-password <user-id>` mints a one-shot temporary password and revokes every session — refused (`403`) for your own account when the CLI presents a personal access token, which is what a login stores: change your own password from the studio (Account → Profile → Change password); `erase <user-id> --yes` erases an account by anonymization (GDPR art. 17) — irreversible, so refused without `--yes`; refused on your own account and for the last owner of a shared organization, safe to repeat; see [cloud-admin-guide.md §1.11](cloud-admin-guide.md#111-erase-an-account-right-to-erasure-gdpr-art-17). See [administering accounts](ticket-context.md#an-account-signs-in-and-sees-nothing). |
| `admin` | `orgs · users · teams · runner-pools · dlq · llm · caps · bots · roles · sandbox` (super-admin; `llm api-keys`/`llm oauth` = the platform fallback credentials — rotate without a redeploy, see [cloud-llm-credentials.md](cloud-llm-credentials.md); `caps` = the runtime usage-cap percentages — retune without a restart, see [usage-caps.md](usage-caps.md#changing-the-caps-at-runtime-no-restart); `bots` = platform bot overrides — push any bot without an image rollout, `roles`/`sandbox` = runtime webhook role bindings + `sandbox: auto` image, see [platform-bots.md](platform-bots.md)) |
| `admin runner-pools` / `admin teams` | `runner-pools list` and `set --data` replace the registry; `teams runner-pool TEAM --pool NAME\|--clear [--force]` and `teams llm-fallback TEAM --policy none\|platform\|--clear` use the audited mapping/policy routes. See [pool cutover and host projection](sovereign-pools.md#operations). |
| `admin dlq` | `list · show · replay · delete`, each with optional `--pool NAME`; sequence arguments are positive integers. Omit the flag for the shared DLQ. |
