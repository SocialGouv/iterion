# Sentry feedback loop — reading production errors back into development

[docs/observability.md](observability.md) covers the *emitting* half: what the
server/runner/dispatcher send to Sentry (panics, fatal exits, error logs, run
alerts, transactions) and which env vars turn it on. This runbook covers the
*consuming* half: how an iterion developer (or an agent session) reads those
errors back — to triage, reproduce and fix them — and the path toward an
automated detect→fix loop.

## Where production errors live

The reference deployment reports to a **Sentry instance operated by the
platform team** (iterion is a tenant, not the operator of the instance):

- Host: `sentry2.fabrique.social.gouv.fr` (self-hosted Sentry, v24.x)
- Organization slug: `incubateur`
- Project: **`iterion`** (id 62, platform `go`) — one project for all
  surfaces; `SENTRY_ENVIRONMENT` distinguishes `production` from local.

Both the server and runner deployments carry the DSN (verified on prod:
`SENTRY_DSN` on `deploy/iterion` AND `deploy/iterion-runner`,
`SENTRY_TRACES_SAMPLE_RATE=0.1`). If a crash does not appear there, suspect
the wiring before the code — see the smoke tests in
[observability.md](observability.md).

## One-time setup: a user auth token

Everything below needs a **User Auth Token** (each dev creates their own):

1. Sign in on `https://sentry2.fabrique.social.gouv.fr` → **Settings →
   Account → API → User Auth Tokens → Create New Token**.
2. Scopes: **Project: Read**, **Issue & Event: Read & Write** (Write is what
   allows resolving/assigning from the loop), **Organization: Read**.
3. The token (`sntryu_…`) is shown ONCE. Store it outside any repo (e.g. a
   private secrets file) and export it in your shell:

```sh
export SENTRY_ACCESS_TOKEN="$(cat ~/.secrets/sentry2-token.txt)"
```

(Use your own secret path; direnv's `.envrc.local` works well. Never commit
it — the repo's `.mcp.json` only references the variable.)

## MCP server (Claude Code / any MCP client)

The repo ships a project-scoped [.mcp.json](../.mcp.json) that registers two
servers: **iterion's own operator MCP** (`iterion mcp` — the `local_*`/
`remote_*` tools of [mcp-server.md](mcp-server.md); needs the `iterion`
binary on PATH, `task build` + install) and the **official Sentry MCP server** (`@sentry/mcp-server`, works self-hosted via
`--host` + `--access-token`), pinned to the org/project above. With
`SENTRY_ACCESS_TOKEN` exported, any Claude Code session opened in this repo
gets the `sentry` MCP tools after approving the server: search issues, fetch
an issue with its stacktrace, list recent events, update/resolve issues.

Typical session flow:

1. "List the new unresolved issues in iterion since the last release."
2. Pick one → fetch the event: stacktrace frames point at `pkg/...` files.
3. Reproduce with a test, fix, PR through the merge queue (Revi gate).
4. Resolve the Sentry issue referencing the PR — a regression reopens it
   automatically (Sentry's resolved→regressed transition), which is the
   loop's built-in verification.

Node ≥ 18 is required for `npx @sentry/mcp-server` (devbox's `nodejs_24`
qualifies; run sessions from the devbox environment).

## Raw API (scripts, bots, quick checks)

`sentry-cli` is release/sourcemap-oriented; for issues use the REST API:

```sh
S=https://sentry2.fabrique.social.gouv.fr/api/0
H="Authorization: Bearer $SENTRY_ACCESS_TOKEN"

# Unresolved issues, most recent first
curl -sS -H "$H" "$S/projects/incubateur/iterion/issues/?query=is:unresolved&sort=date" \
  | jq -r '.[] | "\(.shortId)\t\(.count)x\t\(.lastSeen)\t\(.title)"'

# One issue's latest event (full stacktrace)
curl -sS -H "$H" "$S/organizations/incubateur/issues/<issueId>/events/latest/" | jq .

# Resolve with a reference
curl -sS -X PUT -H "$H" -H 'Content-Type: application/json' \
  "$S/organizations/incubateur/issues/<issueId>/" -d '{"status":"resolved"}'
```

Filter what the SDK sends per environment with `query=is:unresolved
environment:production`.

## Toward the automated loop (error-watch sentinel)

The manual flow above generalizes into the same shape as vuln-watch (Senti):
a **zero-LLM scheduled sentinel** polls the issues API; a *new or regressed*
issue becomes a board card (dedup by Sentry issue id/fingerprint, stacktrace
in the body, `source:sentry` label); the optional zero-touch lane launches a
fixer bot on the card; the fix PR rides the Revi merge gate; on release the
issue is resolved via the API, so a regression re-opens the card. Detection
costs nothing at rest; every fix stays gated. This bot does not exist yet —
the design intent is recorded here so the next session builds it instead of
re-deriving it.
