# Repo-first studio — the repository scope

Cloud-mode studio reads **Org → Repo**: below the organization switcher,
the sidebar carries a **RepoSwitcher** whose selection scopes most views
and pre-fills every repo-targeting action. Teams (the resource tenant,
ADR-048) stay the authorization boundary but are *escamotées* from the
chrome: team switching lives inside the org menu and only appears when
the org actually has several teams.

## Semantics

- The active repo is a **UI context**, not an authz boundary — every
  API stays team-scoped; the client filters and pre-fills.
- **Repo-first default**: on first load the studio auto-selects the
  last-used repo for this (user, team) — localStorage key
  `iterion.activeRepo.<userID>.<teamID>` — falling back to the first
  connected repo. Actions (launch target, board card, bot enable)
  inherit it pre-filled.
- **"All repos"** is the explicit overview mode: aggregate views;
  actions that need a repo ask inline.
- **Zero repos connected**: the switcher becomes the "Connect a repo…"
  CTA → `/integrations/connect` (the guided wizard).
- A stored repo that got disconnected silently resets to the first
  connected one on the next fetch.

## Data source

`GET /api/teams/{id}/forge/repos` — one row per RepoIntegration joined
with its connection: `connection_id`, `provider`, `repo_full_name`,
`clone_url`, `web_url`, `integration_id`, `bot_ids`,
`sync_issues_enabled`, `connection_status`. Local mode returns `[]`
(the switcher hides itself outside cloud).

## What the scope drives

| Surface | Behaviour when a repo is active |
|---|---|
| Home | Get-started progression + Repositories panel; the Runs panel follows the scope |
| Runs | list filtered by `project_path` (chips stay usable as a local override; the sidebar re-anchors on scope change) |
| Board | cards filtered on `external.repo`; "Include unlinked" toggle; new cards pre-linked to the active repo (IssueModal Repository picker; forge-synced cards read-only) |
| Pipelines | cards filtered on `card.external.repo` (issue linkage, or the run's `project_path` for direct launches); "Include unscoped" toggle; AddTaskDialog links new tasks via the same Repository picker |
| What's next | the session launches against the active repo (`repo_url`+`connection_id` → runner clone), the session key includes the repo (switching repos switches conversations), and the header warns on a scope mismatch |
| Automations | `listTriggers({repo})`; NewTriggerDialog pre-fills the active repo |
| Launch | the "Target repository" section pre-selects the active repo (bots declaring a manifest `repo:` block); a bot's home page states the target up-front |
| Integrations | the active repo's card is the landing focus |
| ⌘K palette | per-repo switch entries + "Connect a repository…" |

## Bot repo requirement (manifest `repo:`)

A bot declares its runtime repository need in `manifest.yaml`:

```yaml
repo:
  mode: optional        # required | optional | none
  allow_create: true    # offer "Create a new repository"
  purpose: "Where the new application will live."
  visibility: private   # default for created repos
```

The Launch form renders it as the **Target repository** section:
active repo → another connected repo → *create a new repo*
(`allow_create`) → none (`mode: optional`). `required` soft-blocks the
launch until a target is picked. Creation calls
`POST /api/teams/{id}/forge/repos` (connection-scoped, creation only —
iterion never updates or deletes forge repositories; GitHub Apps mint a
per-call `administration:write` token, an opt-in grant requested at App
creation).

A repo-targeted launch sends `repo_url` + `connection_id` on
`POST /api/runs`; the server pins the connection's managed forge token
as the run's `forge_token` secret (the same Tier-0 pinning webhook
launches use) and the cloud runner clones the repo before sandboxing.
An **empty** just-created repo is handled end-to-end: the clone
succeeds, `worktree: auto` degrades to in-place on the unborn HEAD, and
the bot's first push publishes the default branch. Local-mode servers
reject repo-targeted launches explicitly (no forge stores → no
credential source).

## Connect wizard

`/integrations/connect` — a resumable, route-based flow (survives the
OAuth/App-install redirects via `?connected=` / `?installed=` appended
by the server callbacks): provider → GitHub App manifest creation
(org + visible "allow repo creation" grant checkbox) or install →
repository pick + bot enable (the shared EnableRepoPanel). A GitHub App
installation whose scope misses the wanted repo no longer dead-ends:
the panel surfaces the installation's live scope with the exact GitHub
settings URL to widen it (`GET .../connections/{id}/health`).
