# ADR-002: Separating the Run console from the Workflow editor

## Context

The Iterion editor is a React/Vite SPA embedded in the Go binary
(`pkg/server/`). It originally hosted a single concern: authoring `.iter`
workflows visually — drag-and-drop on a ReactFlow canvas, inspector
panels for declarations, validation against the IR compiler, save/load
through a REST file API.

When the run console UI was added (Phase 1 = `pkg/runview` backend +
REST endpoints; Phase 2 = WebSocket transport + frontend `RunView`), a
design choice surfaced:

> Should the live execution view be **integrated into the editor**
> (one canvas, one page, with a `mode: edit | observe` toggle), or
> should it live as a **separate route/page** with its own canvas,
> store, and transport layer?

The separation was retained. This ADR documents the reasoning so future
contributors understand why fusing the two views — a tempting
simplification — would be a step backwards.

### Status quo at the time of decision

- The editor canvas is **read-write**: nodes are draggable, edges can be
  created/deleted, declarations are mutable through the inspector,
  undo/redo is wired through `useDocumentStore`.
- The run view requirements are fundamentally different: a
  **read-only** representation of the IR with execution state painted
  on top, an event log streaming at ~1–10 ms per event over a
  WebSocket, a 1 MiB ring buffer for stdout/stderr, time-travel
  scrubbing across thousands of events, and a deterministic snapshot
  loaded once per session.
- Existing examples in the React ecosystem (and inside this repo's own
  `EditorView`) showed that mixing edit and observe semantics on a
  single ReactFlow canvas leads to ambiguous interaction (does a click
  select for editing, or for inspecting an artifact at sequence N?).

## Decision

The run console is implemented as **a separate page** in the same SPA:

- **Routing** (wouter, in [editor/src/App.tsx](../../editor/src/App.tsx)):
  - `/` → `EditorView` (author mode)
  - `/runs/new?file=<path>` → `LaunchView` (launch form)
  - `/runs/:id` → `RunView` (observer mode)
  - `/runs` → `RunListView` (history)

- **Stores** (Zustand, in [editor/src/store/](../../editor/src/store/)):
  - `useDocumentStore` — AST, IR, dirty flag, undo/redo (editor only)
  - `useRunStore` — events, snapshots, execution state, WS connection
    (run view only)
  - The two stores **never share state**. `useRunStore.reset()` is
    called when navigating away from `RunView`.

- **Transport**:
  - Editor: REST endpoints (`/api/files/open`, `/api/validate`,
    `/api/files/save`) — request/response, on-demand, no streaming.
  - RunView: REST snapshot (`/api/runs/{id}`) **plus** a persistent
    WebSocket (`/api/ws/runs/{id}`) that streams events with
    `seq`-based replay and ring-buffered logs.

- **Canvas**:
  - Editor uses an editable `Canvas`
    ([editor/src/components/Canvas/Canvas.tsx](../../editor/src/components/Canvas/Canvas.tsx))
    with full ReactFlow interactivity.
  - RunView uses `RunCanvasIR`
    ([editor/src/components/Runs/RunCanvasIR.tsx](../../editor/src/components/Runs/RunCanvasIR.tsx)),
    a distinct read-only component that
    paints execution state (running, succeeded, failed, paused) onto
    the IR graph and supports time-travel scrubbing — but no mutations.

- **Navigation between modes** is explicit: clicking "Launch" in the
  editor calls `setLocation('/runs/new?file=…')`; after run creation,
  the LaunchView navigates to `/runs/{run_id}`. Returning to the
  editor uses `setLocation('/')`. Deep-links carry context
  (`?from={runId}`, `?node={irNodeId}`) so the user can move between
  the two without losing thread.

## Alternatives considered

### 1. Single page with a `mode: edit | observe` toggle

One canvas, one route, one store. A toggle (or implicit detection
based on the URL) switches between editing the document and observing
a run.

**Rejected because**:
- A single ReactFlow canvas would have to support both **mutation
  events** (drag node, draw edge) and **observation events** (click
  to inspect artifact at seq N), with the same handlers. Disambiguating
  these on every gesture multiplies the click-handling logic.
- A single store would either need disjoint sub-trees (essentially
  re-implementing the current separation with extra glue) or merged
  state — and merging means a stray edit dispatched during a live run
  could mutate the document the run is observing.
- Throttling, virtualization, and ring-buffer logic for events would
  have to be conditionally enabled, leaking run-view concerns into the
  authoring experience.

### 2. Side-by-side split-pane (editor left, run right, simultaneous)

The editor and the run console rendered in two panes of the same page,
both alive at once.

**Rejected because**:
- The observed usage pattern is **sequential**: author → launch →
  observe → return to author. Side-by-side optimizes for a usage that
  doesn't match how authors actually work.
- Screen real estate cost: both views want generous canvas space,
  inspector panels, and event logs. Splitting halves both.
- Focus management becomes ambiguous (keyboard shortcuts, undo, "Esc"
  to deselect — which pane owns them?).
- A future split mode can be **composed from the existing separated
  views** (a parent layout that mounts both routes) without breaking
  the isolation guarantees.

### 3. Modal/drawer overlay over the editor

Open the run view as a slide-over panel above the editor.

**Rejected because**:
- A run can last minutes to hours. A modal is the wrong primitive for
  a long-lived, deep-linkable, refresh-resilient view.
- Modals don't survive page reloads — but `RunView` must, since
  operators frequently bookmark `/runs/{id}` to follow long runs.

## Arguments in favor

### 1. Orthogonal concerns

Editing structure (AST mutations, validation, save/load) and
visualizing execution state (event streams, scrubbing, logs) are
genuinely different domains. They share no business logic, they have
no overlapping invariants, and they evolve independently.

### 2. Incompatible data models

| Aspect             | Editor                      | RunView                                 |
|--------------------|-----------------------------|-----------------------------------------|
| Source of truth    | AST + IR                    | Sequenced event stream                  |
| Refresh trigger    | User action (validate/save) | WebSocket push (~1–10 ms)               |
| Persistence model  | File save via REST          | Append-only events.jsonl (read-only)    |
| Mutation surface   | Full (CRUD on nodes/edges)  | None                                    |
| Memory pressure    | Bounded by file size        | Bounded by ring buffer + virtualization |

A unified state container would have to special-case nearly every
operation. Two stores stay simple.

### 3. Store isolation as a safety property

Because `useDocumentStore` and `useRunStore` are physically separate
Zustand instances, no editor action can ever mutate run state and
vice-versa. This is a load-bearing guarantee: a run is pinned to a
specific workflow hash, and accidental edits during observation would
break that contract. Separation enforces it at the type level.

### 4. Canvas semantics stay clean

In `EditorView`, clicking a node means "select for inspection/edit".
In `RunView`, clicking a node means "show me this node's artifacts at
the current sequence". Sharing a canvas would force every handler to
branch on mode. Two canvases keep each interaction model crisp.

### 5. Deep-links preserve flow

Navigation friction is minimal because the editor and the run view
exchange contextual deep-links:
- Editor → RunView: "Launch" → `/runs/new?file=…`
- RunView → Editor: "Open in editor" → `/?file=…&node=…&from={runId}`
- The `?from={runId}` query param triggers a banner in the editor
  ("Coming from run X — see in console") so the user keeps thread.

### 6. Independent scaling

`RunView` ships dedicated optimizations — virtualized event log, ring
buffer for logs, throttled WebSocket batching — without bloating the
editor bundle or complicating its render path. The editor remains lean
and focused.

## Arguments against

### 1. No live-edit during a run

A user cannot tweak a `.iter` file while observing a running execution
of it. **This is intentional**: the run is pinned to a specific
workflow hash via the store, and editing the source mid-run has no
clear semantics (does the change apply? to future iterations only? to
already-scheduled branches?). The current model — edit, save, launch a
new run — is unambiguous and matches the engine's actual behavior.

### 2. No native side-by-side

If a user wants to watch a run while editing a different workflow,
they need two browser tabs. This is a real but minor cost. If demand
grows, a parent split layout can compose the two existing views
without altering them.

### 3. Two ReactFlow components instead of one

`Canvas` (editor) and `RunCanvasIR` (run view) duplicate some
boilerplate (ReactFlowProvider setup, layout helpers). The duplication
is acceptable: the requirements diverge enough (editable vs. read-only,
domain-specific overlays) that a shared component would accumulate
configuration flags faster than it would save lines.

### 4. Minor chrome duplication

Each view ships its own header, breadcrumb, and toolbar. Minimal cost;
shared atoms (buttons, icons) are factored where appropriate.

## Consequences

- The editor evolves freely (new node types, new inspector panels,
  validation rule changes) without considering run-view invariants.
- The run view evolves freely (new event types, new metrics panels,
  protocol changes) without considering authoring invariants.
- Adding a side-by-side mode in the future is a **composition
  problem**, not a refactor: a parent layout can mount both routes in
  separate panes without changing either view's internals.
- The decision should be revisited if a clear use case for
  live-edit-while-running emerges — but the engine's hash-pinned
  execution model makes this unlikely in v1.

## References

- Routing: [editor/src/App.tsx](../../editor/src/App.tsx)
- Editor view: [editor/src/components/EditorView.tsx](../../editor/src/components/EditorView.tsx)
- Run view: [editor/src/components/Runs/RunView.tsx](../../editor/src/components/Runs/RunView.tsx)
- Launch view: [editor/src/components/Runs/LaunchView.tsx](../../editor/src/components/Runs/LaunchView.tsx)
- Run list: [editor/src/components/Runs/RunListView.tsx](../../editor/src/components/Runs/RunListView.tsx)
- Stores: [editor/src/store/document.ts](../../editor/src/store/document.ts), [editor/src/store/run.ts](../../editor/src/store/run.ts), [editor/src/store/ui.ts](../../editor/src/store/ui.ts)
- Backend snapshots & WS: [pkg/runview/](../../pkg/runview/)
- HTTP routes: [pkg/server/](../../pkg/server/)
