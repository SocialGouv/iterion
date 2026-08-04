# The iterion MCP server — `iterion mcp`

`iterion mcp` serves the **operator-facing MCP server** on stdio: any
MCP client (Claude Code, Claude desktop, Cursor, an agent SDK) gets a
tool surface to drive iterion end to end — both the **local** engine
and store on this machine, and a **remote** instance you are logged in
to via `iterion remote`.

It is distinct from the hidden `__mcp-*` servers (`__mcp-board`,
`__mcp-ask-user`, `__mcp-control`), which are internal per-run
transports the engine wires up for its own bots. It is also the
complement of the [agent skill](skill.md): the skill teaches an agent
to *author* `.bot` workflows; the MCP server gives it typed tools to
*operate* iterion (launch, follow, answer, board, cloud).

## Setup

### Claude Code

```sh
# local scope (this project, private to you) — the default:
claude mcp add iterion -- iterion mcp

# project scope (committed .mcp.json, shared with the team):
claude mcp add --scope project iterion -- iterion mcp
```

Or hand-written in a project's `.mcp.json`:

```json
{
  "mcpServers": {
    "iterion": { "command": "iterion", "args": ["mcp"] }
  }
}
```

### Claude desktop

`claude_desktop_config.json` → `mcpServers`, same shape as `.mcp.json`
above. Point `command` at an absolute `iterion` path if the desktop
app's PATH is minimal.

### Cursor / other MCP clients

Any client that can spawn a stdio MCP server works: command `iterion`,
args `["mcp"]`, working directory = the project you want the `local_*`
tools anchored on.

### Anchoring and flags

The server anchors on its **working directory**: the local store
resolves exactly like the CLI (`<workdir>/.iterion` when it is a
managed store, else the per-project slot under
`~/.iterion/projects/<key>/`), and relative paths in tool arguments
resolve against that directory.

| Flag | Effect |
|------|--------|
| `--store-dir <dir>` | Pin the run store the `local_*` tools operate on. |
| `--read-only` | Expose only read tools. Mutating tools disappear from `tools/list` and are refused; `remote_api` stays available but GET-only. Nothing is ever created on disk: an absent store/board is an explicit error instead of an implicit init. |
| `--only local\|remote` | Expose a single family. |

Every tool carries the MCP `readOnlyHint` annotation, and the
annotation is **truthful about capability** (so clients can gate
auto-approval on it): `remote_api` reports `readOnlyHint: false` even
though read-only mode restricts it to GET.

## The two tool families

### `local_*` — this machine

| Tool | Access | What it does |
|------|--------|--------------|
| `local_validate` | read | Parse/compile/validate a `.bot` or `.botz` (diagnostics JSON; `valid:false` is a normal answer). |
| `local_bots_list` | read | Discover bots under `bots/`, `examples/` (or given paths). |
| `local_runs_list` | read | List runs in the store (status/workflow filters, newest first). |
| `local_run_get` | read | One run's record: status, error, budget, worktree finalization (`final_commit`/`final_branch`/`merged_into`), and for `running` docs an `executing` liveness verdict (see below). |
| `local_run_events` | read | Structured event stream, incremental via `since`. |
| `local_run_log` | read | Plain-text `run.log` tail. |
| `local_run_report` | read | The chronological markdown report (`iterion report`). |
| `local_questions` / `local_answer` | read / write | Pending async questions (ADR-081) and their answers (empty answers allowed). |
| `local_run` | write | **Launch a run** (see semantics below). |
| `local_resume` | write | Resume a paused / `failed_resumable` / cancelled run (answers supported). |
| `local_run_cancel` | write | Cancel a detached run — or explicitly repair a dead one (see below). |
| `local_board_*` | read/write | The native kanban board of this store — the full boardops tool set (`create_issue`, `list_issues`, `get_issue`, `transition_issue`, `assign_issue`, `set_bot`, `set_labels`, `comment_issue`, `close_issue`, `list_labels`) under the `local_board_` prefix, all capabilities granted (the operator drives their own board). |

**Launch semantics (`local_run`).** The workflow is validated
synchronously (invalid → diagnostics, nothing launched), then executed
by a **detached** `iterion run --background` subprocess in its own
session:

- the run **survives the MCP client/server exiting** — closing your
  Claude Code session does not kill a running bot;
- it lands in the same store the studio reads, so it is fully visible
  there (shared-store integration, no server required);
- backend/credential resolution is exactly a manual `iterion run` from
  that directory (the subprocess inherits the environment).

**Liveness and cancel semantics.** The run's **flock** (held by
whichever live process executes the run, released by the OS on any
death) is the liveness oracle; the `.pid` file only names the process
to signal. `local_run_get` reports `executing: true|false` on running
docs and warns when nothing holds the lock. `local_run_cancel`:

- lock held + recorded pid alive → SIGTERM to the runner's process
  group (normal cancel; poll `local_run_get` until `cancelled`);
- lock held but no usable `.pid` → refused: the run is executing under
  another surface (studio, a terminal) — cancel it there;
- lock free + recorded pid dead → the runner died without a terminal
  status (SIGKILL, reboot): explicit repair — the run is marked
  `failed_resumable`, the stale `.pid` removed, everything reported;
- lock free + recorded pid alive → ambiguous (runner still booting, or
  the pid number was recycled by an unrelated process): refused, retry
  shortly — signalling would risk killing a stranger.

### `remote_*` — the logged-in instance

Authentication reuses the `iterion remote` credential: run
`iterion remote login <url>` once (browser flow), or set
`ITERION_REMOTE_URL` + `ITERION_REMOTE_TOKEN` (CI / headless — see
[cloud-cli.md](cloud-cli.md)). Credentials are re-resolved **on every
call**, so logging in/out or switching teams takes effect without
restarting the server. No credential → explicit error telling you how
to log in.

| Tool | Access | What it does |
|------|--------|--------------|
| `remote_status` | read | Instance + account + active org/team. |
| `remote_runs_list` / `remote_run_get` / `remote_run_events` / `remote_run_log` / `remote_run_artifacts` | read | Follow cloud runs. |
| `remote_bots_list` / `remote_bots_get` | read | The instance's bot catalog. |
| `remote_issues_list` | read | The remote native board. |
| `remote_routes` / `remote_openapi` | read | **Discovery**: the live route table and OpenAPI spec (always pass `path_prefix` — the full spec is large). |
| `remote_runs_launch` | write | Launch from a catalog `bot_id` or a local `.bot` sent inline. |
| `remote_runs_resume` / `remote_runs_cancel` | write | Lifecycle actions. |
| `remote_issue_create` / `remote_issue_update` / `remote_issue_transition` / `remote_issue_comment` | write | Board mutations (update is a strict PATCH: only provided fields change). |
| `remote_api` | escape hatch | **Any** authenticated request (`method` + `path` + JSON `body`) — the MCP twin of `iterion remote api`. With `remote_routes`/`remote_openapi` this makes the whole cloud surface reachable without a dedicated tool per endpoint. GET-only under `--read-only`. |

## How-to / cookbook

Things to just ask your agent once the server is registered:

**Launch and babysit a local bot.**
> "Validate `bots/docs-refresh/main.bot`, launch it with
> `post_to_board=false`, and keep me posted until it finishes."

The agent chains `local_validate` → `local_run` → polls
`local_run_get` / `local_run_events` (with `since`) → summarizes with
`local_run_report`. The run keeps going even if you close the session;
any later session can pick it back up from the store.

**Unblock a paused run.**
> "Any iterion run waiting on me? Answer the release question with
> 'ship it'."

`local_runs_list {status: paused_waiting_human}` → `local_questions` →
`local_answer` (or `local_resume` with `answers` for a paused human
form).

**Post-mortem a failed run.**
> "Why did run X fail?"

`local_run_get` (error + `executing` verdict) → `local_run_events` /
`local_run_log {tail}` → `local_run_report`. If the runner died
without a terminal status, `local_run_cancel` repairs it to
`failed_resumable` and `local_resume` continues it.

**Groom the kanban.**
> "Create a board issue for the flaky webhook test, label it `bug`,
> route it to feature-dev."

`local_board_create_issue` → `local_board_set_labels` →
`local_board_set_bot` (the dispatcher picks it up from there — see
[native-tracker.md](native-tracker.md)).

**Drive the cloud instance.**
> "Launch the review-pr bot on the cloud for repo X and follow it."

`remote_status` (sanity) → `remote_runs_launch {bot_id: "review-pr",
vars: …}` → `remote_run_events`. For anything not covered by a typed
tool: `remote_routes {filter}` → `remote_api` — e.g. *"how much did my
org spend this month?"* resolves to `remote_api GET /api/orgs/…/usage`.

**A read-only observer agent.**
Register a second, restricted server for agents that must not act:

```sh
claude mcp add iterion-ro -- iterion mcp --read-only
```

## Headless / CI

The server itself is headless; only the remote credential needs
provisioning. Environment-only (no config file, token never sent to a
different host):

```sh
export ITERION_REMOTE_URL=https://iterion.example.com
export ITERION_REMOTE_TOKEN=iap_…   # a PAT
```

`--only remote` gives such an agent zero local-store access.

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| Remote tools error with "not logged in" | Run `iterion remote login <url>` once, or set `ITERION_REMOTE_URL` + `ITERION_REMOTE_TOKEN`. Credentials are re-read on every call — no server restart needed. |
| `local_runs_list` shows nothing while the studio shows runs | The MCP server anchored on a different store. Check the `store_dir` echoed in every local result; pin it with `--store-dir` (see [Store resolution](persisted-formats.md)). |
| `local_run` fails with "locate iterion binary" | The server spawns the runner via the installed `iterion` binary (same resolution as the studio's detached mode). Install one on PATH — and keep it fresh, a stale binary runs stale engine code. |
| `local_run_get` says `executing: false` on a running doc | The runner died without reaching a terminal status (SIGKILL, reboot). `local_run_cancel` repairs it to `failed_resumable`; `local_resume` continues it. |
| `local_run_cancel` refuses with "ambiguous state" | The recorded pid is alive but nothing holds the run lock — runner still booting or pid recycled. Retry in a moment; the refusal is what keeps a recycled pid from being signalled. |
| The server exits after a huge request | A single JSON-RPC line beyond 4 MB poisons the stdio framing; the server sends a `-32700` explaining it, then exits. Keep large payloads (vars, answers) in files and pass paths. |

## Security notes

- `remote_api` wields the **full power of the stored PAT** (team/org
  scope included). Hand an agent `--read-only` when it only needs to
  observe, or a PAT minted for a restricted team.
- The local family holds the same power as your shell running
  `iterion …` in that directory — it is not a sandbox. `--only remote`
  exposes no local store access at all.
- Secret VALUES never transit: the remote endpoints already return
  masked/redacted secret metadata, and no local tool reads secret
  stores.

## Follow-ons (not yet implemented)

- HTTP transport on `pkg/server` (`/api/mcp`, PAT/JWT, tenant-scoped)
  so hosted agents can connect without the binary — the dispatch in
  `pkg/operatormcp` is transport-agnostic by construction.
- MCP resources (`iterion://runs/<id>/report`) and prompts.
