[← Documentation index](README.md) · [← Iterion](../README.md)

# Visual Editor (web)

**The same workflows, visually.** Build on the canvas or edit the `.bot` source directly — both stay in sync.

Iterion includes a browser-based visual workflow editor built with React and XYFlow. Served by your local `iterion` binary — no installation beyond the CLI.

![Iterion studio — visual workflow editor with canvas, node library, and inspector](images/studio/editor-canvas.png)

```bash
iterion studio                     # Launch on default port (4891), opens browser
iterion studio --port 8080         # Custom port
iterion studio --dir ./workflows   # Custom working directory
iterion studio --bind 0.0.0.0      # Expose on the LAN (default 127.0.0.1)
iterion studio --bots-path ./bots  # Add a bot discovery path (repeatable; feeds the Launch modal)
iterion studio --no-browser        # Don't auto-open browser
iterion studio --no-browser-pane   # Disable the run console's Browser pane
```

See [cli-reference.md `#iterion-studio`](cli-reference.md#iterion-studio) for the full flag set
(networking, attachments, bot discovery).

## What you get

- **Bot builder (`/bots/new`)** — Guided creation of a new bot, from template gallery to first test run (see below)
- **Canvas** — Drag-and-drop node graph with auto-layout, zoom, search, and keyboard shortcuts
- **Node library** — Drag pre-built node types (agent, judge, router, human, tool, compute) onto the canvas
- **Property editor** — Edit node properties, schemas, prompts, and edge conditions in a side panel
- **Source view** — Split-pane view showing the raw workflow source (`.bot`) alongside the visual graph
- **Live diagnostics** — Real-time validation errors and warnings as you edit (sparse DSL range C001–C199; bundle checks C200–C230)
- **File watching** — Detects external file changes via WebSocket and syncs automatically
- **Undo/redo** — Full edit history
- **Launch modal** — Fills `vars` and attachments at launch time, with bot/argument discovery driven by `--bots-path` (the modal's bot picker and argument form consume the same catalogue `iterion bots list` emits)
- **Kanban `/board` view** — Native tracker CRUD with drag-and-drop (gated on `server_info.native_tracker_enabled`; see [native-tracker.md](native-tracker.md))
- **Pipeline `/pipelines` board** — Global control-center board tracking staged tasks and their in-flight runs, with priority-driven launch (same `server_info.native_tracker_enabled` gate; concurrency capped by `--max-concurrent-pipelines`; see [native-tracker.md](native-tracker.md))
- **`/dispatcher` dashboard** — Live running + retry tables when `iterion dispatch` is wired (gated on `server_info.dispatcher_enabled`; see [dispatcher.md](dispatcher.md))
- **Browser pane** — Preview URLs, live CDP screencast, and time-travel screenshots tied to a run (see [browser-pane.md](browser-pane.md)). Disable with `--no-browser-pane`.
- **Run console** — Launch a workflow from the studio and watch events stream live

This mode is the simplest way to design and iterate locally. If you want a packaged native window instead (no browser, OS-keychain credentials, auto-update), see the [Desktop App](desktop.md).

## Creating a bot (`/bots/new`)

The **New bot** button on the Bots view opens the guided builder — the
recommended way to start a bot from nothing. It runs in two phases on a
single page:

1. **Template gallery** — pick a starting point (`blank`, `daily-digest`,
   `code-reviewer`, `docs-writer`, `issue-triager`). Each pre-fills the
   form; every field stays editable afterwards.
2. **Form → create → test** — name the bot, write its mission, set the
   optional dials (model/backend, worktree, sandbox, permission, budgets,
   a suggested cron), then create it. An embedded test-run pane lets you
   iterate without leaving the page. Drafts auto-save to local storage.

Creating writes a real bundle to `<workdir>/bots/<slug>/` — `main.bot`,
`manifest.yaml`, `README.md`, and the `skills/ prompts/ attachments/
presets/` layout — and refreshes the generated bot catalogue, so the new
bot is immediately discoverable, launchable, and dispatchable. The
generated workflow is parsed **and** compiled and the manifest strictly
decoded *before* anything lands on disk, so the builder cannot write a
broken bundle.

The CLI equivalent is `iterion bots create <slug> --template <id>`; both
surfaces render through the same `pkg/botscaffold` engine, so a bot
created either way is identical. See the
[CLI reference](cli-reference.md#iterion-bots).

> Local mode only. In cloud mode the builder is disabled (bots are
> authored through git) and the view points you at the catalogue and
> marketplace instead.

## Screenshots

> All shots use the studio's dark theme; a light theme is available from Settings → Appearance or `⌘/Ctrl+K → Cycle theme`.

### Authoring

**Source view** — the raw `.bot` source mirrored beside the graph, edits in either stay in sync.

![Studio source view — graph and .bot source side by side](images/studio/editor-source.png)

**Launch modal** — fills `vars` and attachments, picks a backend, and previews the estimated cost before the run starts.

![Studio launch modal with vars form, backend selector, and cost preview](images/studio/launch.png)

### Running & observing

**Run console** — a live graph of the run, a streaming event log, and a header showing the commit/branch the run landed on.

![Studio run console — live IR graph and streaming logs](images/studio/run-console.png)

**Run in flight** — a running review with both cross-family judges executing in parallel (note the green *Running* badge and the live log tail).

![Studio run console — a run in progress with parallel judge branches](images/studio/run-live.png)

**File diff** — every file the run changed, opened in a side-by-side diff.

![Studio run console — side-by-side file diff](images/studio/run-diff.png)

**Commits & merge** — the run's commits with a one-click squash-and-merge into your branch.

![Studio run console — commit list with squash and merge](images/studio/run-commits.png)

**Cost report** — per-provider and per-model cost attribution for a finished run.

![Studio run report tab — cost attribution by provider and model](images/studio/run-report.png)

**Runs list** — every run with status, cost, and duration, filterable and sortable.

![Studio runs list with status, cost, and duration columns](images/studio/runs-list.png)

**Run analytics** (`/insights`) — cost over time stacked by workflow, plus per-workflow run counts, fail rates, and P50/P95 durations.

![Studio run analytics — cost-over-time chart and per-workflow stats](images/studio/insights.png)

### Human in the loop

**Interaction form** — when a run reaches a `human` node it pauses and renders a form (here a deploy-approval gate). See [human-in-the-loop.md](human-in-the-loop.md) for the field-type → widget mapping and the four interaction modes.

![Studio — a human node paused with its interaction form](images/studio/hitl-form.png)

### Orchestration

**Kanban board** (`/board`) — the native tracker with drag-and-drop, labels, priorities, and per-card bot assignees, flowing across columns from inbox to done.

![Studio kanban board with work across inbox, backlog, ready, and in-progress columns](images/studio/board-active.png)

**Dispatcher dashboard** (`/dispatcher`) — live config, in-flight runs, and the retry queue.

![Studio dispatcher dashboard with config card and run tables](images/studio/dispatcher.png)

**What's Next** — a conversational session that surveys the repo, proposes a roadmap, and watches the board it dispatches.

![Studio What's Next session with watch panel and chat](images/studio/whats-next.png)

### Workspace & configuration

**Home** — the start page: a What's Next entry, the bot catalog, and recent runs.

![Studio home page with bots and recent runs](images/studio/home.png)

**Command palette** (`⌘/Ctrl+K`) — jump to any view, run, or action; cycle the theme.

![Studio command palette with navigation and recent runs](images/studio/command-palette.png)

**Bot catalog** — enable/disable bots and import new ones from a repository.

![Studio bot catalog manager](images/studio/catalog.png)

**Backends** (Settings → Backends) — auto-detected LLM credentials and the resolved default backend.

![Studio settings — detected LLM backend credentials](images/studio/settings-backends.png)

**Appearance** (Settings → Appearance) — theme (System / Light / Dark) and chat-input behaviour.

![Studio settings — appearance and theme picker](images/studio/settings-appearance.png)

**Marketplace** — browse, submit, and install published `.botz` bundles.

![Studio bot marketplace](images/studio/marketplace.png)
