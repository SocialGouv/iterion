# The iterion MCP server — `iterion mcp`

`iterion mcp` serves the **operator-facing MCP server** on stdio: any
MCP client (Claude Code, Claude desktop, Cursor, an agent SDK) gets a
tool surface to drive iterion end to end — both the **local** engine
and store on this machine, and a **remote** instance you are logged in
to via `iterion remote`.

It is distinct from the hidden `__mcp-*` servers (`__mcp-board`,
`__mcp-ask-user`, `__mcp-control`), which are internal per-run
transports the engine wires up for its own bots.

## Setup

```sh
# Claude Code (project or user scope):
claude mcp add iterion -- iterion mcp

# or in a project's .mcp.json:
{
  "mcpServers": {
    "iterion": { "command": "iterion", "args": ["mcp"] }
  }
}
```

The server anchors on its working directory: the local store resolves
exactly like the CLI (`<workdir>/.iterion` when it is a managed store,
else the per-project slot under `~/.iterion/projects/<key>/`), and
relative paths in tool arguments resolve against that directory. Flags:

| Flag | Effect |
|------|--------|
| `--store-dir <dir>` | Pin the run store the `local_*` tools operate on. |
| `--read-only` | Expose only read tools. Mutating tools disappear from `tools/list` and are refused; `remote_api` stays available but GET-only. |
| `--only local\|remote` | Expose a single family. |

Every tool carries the MCP `readOnlyHint` annotation so clients can
apply their own policies.

## The two tool families

### `local_*` — this machine

| Tool | Access | What it does |
|------|--------|--------------|
| `local_validate` | read | Parse/compile/validate a `.bot` or `.botz` (diagnostics JSON; `valid:false` is a normal answer). |
| `local_bots_list` | read | Discover bots under `bots/`, `examples/` (or given paths). |
| `local_runs_list` | read | List runs in the store (status/workflow filters, newest first). |
| `local_run_get` | read | One run's record: status, error, budget, worktree finalization (`final_commit`/`final_branch`/`merged_into`), runner liveness for detached runs. |
| `local_run_events` | read | Structured event stream, incremental via `since`. |
| `local_run_log` | read | Plain-text `run.log` tail. |
| `local_run_report` | read | The chronological markdown report (`iterion report`). |
| `local_questions` / `local_answer` | read / write | Pending async questions (ADR-081) and their answers. |
| `local_run` | write | **Launch a run** (see semantics below). |
| `local_resume` | write | Resume a paused / `failed_resumable` / cancelled run (answers supported). |
| `local_run_cancel` | write | SIGTERM the detached runner's process group (needs the run's `.pid`). |
| `local_board_*` | read/write | The native kanban board of this store — the full boardops tool set (`create_issue`, `list_issues`, `get_issue`, `transition_issue`, `assign_issue`, `set_bot`, `set_labels`, `comment_issue`, `close_issue`, `list_labels`) under the `local_board_` prefix, all capabilities granted (the operator drives their own board). |

**Launch semantics (`local_run`).** The workflow is validated
synchronously (invalid → diagnostics, nothing launched), then executed
by a **detached** `iterion run --background` subprocess in its own
session:

- the run **survives the MCP client/server exiting** — closing your
  Claude Code session does not kill a running bot;
- it lands in the same store the studio reads, so it is fully visible
  there (shared-store integration, no server required);
- the `.pid` file written at spawn is what `local_run_cancel` and the
  liveness probe in `local_run_get` key on;
- backend/credential resolution is exactly a manual `iterion run` from
  that directory (the subprocess inherits the environment).

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
