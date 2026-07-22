# Cloud CLI — `iterion remote`

`iterion remote` turns the local CLI into a full client for a remote
iterion instance (cloud or self-hosted `iterion server`): every
operator and admin capability of the HTTP API is reachable as a typed
subcommand. The raw escape hatch `iterion remote api <METHOD> <path>`
remains for anything not wrapped; `iterion remote routes` /
`iterion remote openapi` enumerate the live surface.

All commands honour the global `--json` flag: the output is then the
server's response body verbatim (lossless, stable for `jq`).

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

### Notable flags

The typed tree stays terse; `--help` on any leaf is authoritative. A few
non-obvious flags worth calling out:

- `runs launch --callback-url <url> [--callback-token <tok>]` — register a
  **completion webhook**: the instance POSTs the run outcome to `<url>`
  when it finishes, echoing `--callback-token` back so the receiver can
  authenticate the delivery. The fire-and-forget alternative to `--follow`.
- `runs resume <id> --answers @answers.json` — resume a run paused on a
  human/async question by supplying the answers file inline.
- `runs files <id> <path>` reads by default; `--content` prints the file
  body, `--diff` shows its diff, and `--edit @<local-file>` writes the
  local file's bytes back to `<path>` in the run workspace.
- `runs merge <id> --into <branch>` sets the merge **target** branch
  (pairs with `--strategy`); `--message <msg>` overrides the merge commit
  message. `runs list --limit N` (and `audit --limit N`) caps result count.
- `runs launch --model-overrides <json>` supplies a model-overrides JSON
  array (a literal string, or `@file` to read it from a path) applied to the
  launched run.
- `runs delete <id> --yes` confirms the deletion non-interactively.
- `issues comment <id> <text> --transition-to <state>` moves the issue to
  `<state>` right after posting the comment.
- `teams invitations create <email> --role <role>` (and the identical
  `orgs invitations create`) sets the invited member's role (default
  `member`).
- `marketplace moderation reject <slug> --reason <text>` records the
  rejection reason on a marketplace submission.
- `tokens create --expires-days N` (0 = platform default) and
  `api-keys create --default` (make this the provider's default key)
  tune the credential each command mints.

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
| `teams` | `list · create · switch · members · invitations` |
| `orgs` | `list · switch · members · invitations · usage · teams` |
| `me` | `password · sessions-revoke-all · sso-links` |
| `tokens` | `list · create · revoke` |
| `secrets` / `api-keys` | `list · set/create · rotate/update · delete` (`--scope team\|me`) |
| `bindings` | per-bot secret bindings (`list · create · delete`) |
| `webhooks` | `list · get · create · update · delete · rotate · deliveries` |
| `forge` | `connections · repo-bots · oauth-apps · integrations` |
| `audit` / `usage` / `limits` | `audit team\|org\|admin` · org usage · cost limits |
| `memory` | `usage · docs · doc get\|put\|delete · export · import` (`--name` space) |
| `admin` | `orgs · users · dlq` (super-admin) |
| `sso` | `providers · domains` (org-scoped) |
| `plugins` | `list · enable · disable · install · uninstall · config` |
| `server` | `info · health` |

Structured mutation payloads follow the `--data '<json>'` /
`--data @file.json` / `--data @-` (stdin) convention shared with
`remote api`.

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
