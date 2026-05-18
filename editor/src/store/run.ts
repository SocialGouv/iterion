import { create } from "zustand";

import {
  loadEvents,
  type ExecutionState,
  type RunEvent,
  type RunSnapshot,
  type RunHeader,
} from "@/api/runs";
import {
  extractTodosFromInput,
  type TodoItem,
} from "@/components/Runs/toolFormatters";

// NoEventsSeq mirrors runview.NoEventsSeq on the Go side. Disambiguates
// "no events have been applied" from "the most recent event has seq 0".
export const NO_EVENTS_SEQ = -1;

// MAX_EVENTS caps in-memory history so a long-running run with thousands
// of events doesn't bloat the React store. Older events fall off the
// front; the scrubber (Phase 5) will refetch via /events?from=&to= when
// it needs them.
const MAX_EVENTS = 5000;

// inflightHistoryFetches keys runId → in-flight history fetch promise.
// Module-level (not inside the store) so callers that arrive while a
// fetch is in flight await the same promise instead of starting their
// own, even when they slip past the optimistic `historyFetchedForRun`
// marker due to React 18's concurrent rendering.
const inflightHistoryFetches = new Map<string, Promise<void>>();

// MAX_LOG_BYTES caps the in-memory log tail so a verbose run doesn't
// bloat the React heap. Older bytes fall off the front; the start
// offset advances accordingly. Matches the backend ring of 1 MiB so
// the WS replay window stays consistent.
const MAX_LOG_BYTES = 1 << 20;
// Truncate down to LOG_TRIM_TARGET (75% of cap) instead of the cap
// itself so we don't pay an O(N) slice on every appended chunk once
// the cap is reached — amortises the copy to one trim per ~256 KiB.
const LOG_TRIM_TARGET = (MAX_LOG_BYTES * 3) >> 2;

export type WsState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

export interface PendingHumanInput {
  interaction_id?: string;
  node_id?: string;
  questions?: Record<string, unknown>;
  // Absent when rehydrated from RunHeader.checkpoint (reload mid-pause).
  raw?: RunEvent;
}

// QueuedUserMessage mirrors store.QueuedUserMessage on the Go side
// (pkg/store/user_messages.go). It carries one operator chat message
// queued against a running agent and is fanned out via the
// user_message_* event family.
export type QueuedMessageStatus =
  | "queued"
  | "delivered"
  | "consumed"
  | "cancelled";

export interface QueuedUserMessage {
  id: string;
  run_id?: string;
  text: string;
  queued_at: string;
  delivered_at?: string | null;
  consumed_at?: string | null;
  cancelled_at?: string | null;
  status: QueuedMessageStatus;
}

// PreviewSource tracks where the current preview URL came from. The
// distinction matters for the Browser pane UI: workflow-emitted URLs
// auto-show the pane and refresh on every new event, while manual
// URLs entered by the user persist until they clear them.
export type PreviewSource = "tool-stdout" | "manual" | "runtime";

// PreviewScope tells the Browser pane whether to embed the URL
// directly (`external` — relies on the target site's framing
// permissions) or proxy it through `/api/runs/:id/preview` to strip
// frame-blocking headers (`internal` — only safe for URLs the run
// itself published or for content the editor controls).
export type PreviewScope = "internal" | "external";

// BrowserScreenshot is a single saved frame, persisted as a run
// attachment by the runtime (tool-stdout directive today, Playwright
// auto-capture once that path is wired). The list stays sorted
// ascending by seq so the scrubber can pick the latest frame with
// seq <= scrubSeq without copying.
export interface BrowserScreenshot {
  seq: number;
  attachmentName: string;
  url?: string;
  nodeId?: string;
  toolCallId?: string;
}

// BrowserSessionInfo is the live-mode session reference. Set by the
// reducer when an EventBrowserSessionStarted lands; cleared on
// EventBrowserSessionEnded. Drives the BrowserPane's mode toggle:
// when present, the pane defaults to live (canvas screencast); when
// absent, it stays in viewer mode (iframe of currentUrl).
export interface BrowserSessionInfo {
  sessionId: string;
  nodeId?: string;
  startedAt?: string;
}

export interface BrowserPaneState {
  currentUrl: string | null;
  scope: PreviewScope;
  source: PreviewSource | null;
  kind?: string;
  // Latest preview_url event seq the reducer has consumed,
  // regardless of whether it became `currentUrl` (a manual URL
  // takes precedence). Lets the RunView decide when to auto-show
  // the Browser tab.
  lastEventSeqSeen: number | null;
  // Captured screenshots, ascending by seq. Backed by tool-stdout
  // directives today and Playwright tool calls when that path is
  // wired. Capped by MAX_BROWSER_SCREENSHOTS to bound memory on
  // long-running sessions.
  screenshots: BrowserScreenshot[];
  // Active live-mode session, if any. Set by the runtime when a
  // Chromium attaches via the BrowserRegistry, or by the debug
  // attach button.
  liveSession: BrowserSessionInfo | null;
}

// MAX_BROWSER_SCREENSHOTS bounds the in-memory history. The scrubber
// re-fetches via attachment URLs, so dropping older frames is safe;
// only the most-recent N stays available for scrub-without-network.
const MAX_BROWSER_SCREENSHOTS = 200;

const initialBrowserState: BrowserPaneState = {
  currentUrl: null,
  scope: "external",
  source: null,
  kind: undefined,
  lastEventSeqSeen: null,
  screenshots: [],
  liveSession: null,
};

// InFlightTool tracks a tool call the engine has reported as started but
// not yet completed. Populated on `tool_started`, cleared on
// `tool_called` / `tool_error` (matched by toolUseID when present,
// otherwise the oldest entry with the same toolName). Drives the Logs
// panel footer: when any in-flight entry exists for the active filter,
// the random-words "thinking" footer steps aside for a `Running <tool>`
// spinner.
export interface InFlightTool {
  toolName: string;
  // toolUseID correlates start↔completion. Empty when the path doesn't
  // surface one (claw single tool loop, direct tool nodes); in that
  // case completion clears the oldest entry sharing the toolName.
  toolUseID: string;
  // Unix ms when the start event was received — drives the elapsed
  // counter in the footer.
  startedAt: number;
}

// AGENTIC_TOOL_NAMES groups tool names that themselves block on an
// internal LLM call (Claude Code's `Agent`/`Task` sub-agents and claw's
// lowercase `agent`/`task`). They look like a tool to the engine but
// spend almost all their wall-clock waiting on a model, so the UI
// treats them differently: the Logs footer keeps showing the
// random-words "thinking" loader instead of "Running <tool>", and the
// side panel surfaces a count of pending agents.
export const AGENTIC_TOOL_NAMES: ReadonlySet<string> = new Set([
  "Agent",
  "Task",
  "agent",
  "task",
]);

// TODO_LIST_TOOL_NAMES groups tool names that emit a todo list payload.
// Claude Code's SDK uses CamelCase `TodoWrite`; claw exposes the same
// concept as `todo_write` (snake_case). Both write a `todos` array on
// the tool input that we surface in the side panel.
const TODO_LIST_TOOL_NAMES: ReadonlySet<string> = new Set([
  "TodoWrite",
  "todo_write",
]);

// TodoListSnapshot captures the latest todo list a tool call emitted
// for one execution. Populated on `tool_started` (which carries
// `data.input`) and never cleared by sibling tool calls — only a new
// TodoWrite/todo_write call from the same execution replaces it.
export interface TodoListSnapshot {
  todos: TodoItem[];
  updatedAt: number;
  // Source tool name as observed (`TodoWrite` or `todo_write`). The UI
  // doesn't branch on it currently but keeping it lets us label the
  // panel without re-deriving the source from elsewhere.
  source: string;
}

export interface RunLogState {
  // start is the byte offset in the run's logical log stream where
  // text begins. start > 0 means the older bytes were evicted.
  start: number;
  // total is the running write counter the backend reports — total
  // bytes ever written for this run, even those that have rolled out
  // of the in-memory tail. Used by the UI to detect drops.
  total: number;
  text: string;
  // True once this client has subscribed to the live log stream for
  // this run. Independent of whether bytes have arrived yet.
  subscribed: boolean;
  // Set when the backend emits log_terminated — the live stream is
  // over, but the existing text remains for inspection.
  terminated: boolean;
}

interface RunStoreState {
  runId: string | null;
  snapshot: RunSnapshot | null;
  events: RunEvent[];
  // historyFetchedForRun is the runId whose persisted history has
  // been pulled via /events. Null means lazy-mode hydration is still
  // pending. See loadEventHistoryIfMissing for the trigger contract.
  historyFetchedForRun: string | null;
  executionsById: Map<string, ExecutionState>;
  // Most recent exec_id observed per (branch, node) — populated by
  // node_started, consulted by every downstream event that carries
  // node_id (node_finished, tool_*, artifact_written, etc.). Mirror of
  // the backend's SnapshotBuilder.lastExecID. Without this the local
  // reducer had to fall back to a max(loop_iteration) scan to attribute
  // events, which became non-deterministic post-Option-3: nested-loop
  // exec_ids (e.g. fix_loop=0;package_loop=12 vs ...=11) share the
  // same scalar loop_iteration so node_finished could land on the
  // wrong attempt, leaving the actual running exec locked as running
  // forever in the local view (canvas shows half the nodes still
  // running even though the backend snapshot reports them finished).
  lastExecIDByNode: Map<string, string>;
  // In-flight tools, keyed by execution_id. Sorted by start time
  // (insertion order). Empty entries are pruned to keep the map lean.
  inFlightToolsByExec: Map<string, InFlightTool[]>;
  // Latest todo list per execution, populated on TodoWrite/todo_write
  // tool_started events. Cleared when the execution finishes so the
  // side panel only shows live data.
  latestTodosByExec: Map<string, TodoListSnapshot>;
  pendingHumanInput: PendingHumanInput | null;
  // Inbox of operator-queued chat messages for the run. Hydrated via
  // the REST GET /api/runs/{id}/queue-messages endpoint and live-
  // updated through the user_message_* event family.
  queuedMessages: QueuedUserMessage[];
  wsState: WsState;
  followTail: boolean;
  log: RunLogState;
  browser: BrowserPaneState;
  // Increments to request a fresh WS dial. The broker drops a run's
  // subscribers on terminal status (pkg/runview/service.go: CloseRun),
  // so after Resume the still-open WS conn no longer receives events
  // — bumping this token tears down the hook's effect and re-anchors
  // a new subscription against the now-active run.
  wsReconnectToken: number;

  setRunId: (id: string | null) => void;
  setWsState: (state: WsState) => void;
  setFollowTail: (follow: boolean) => void;
  requestWsReconnect: () => void;

  applySnapshot: (snap: RunSnapshot) => void;
  applyEvent: (evt: RunEvent) => void;
  applyEventsBatch: (evts: RunEvent[]) => void;
  // loadEventHistoryIfMissing pulls the persisted event log via the REST
  // /events endpoint and folds it into the store. Idempotent per runId:
  // a second call for the same run is a no-op until reset(). Decoupled
  // from the WS subscribe path so consumers that need history (the
  // EventLog tab, the Scrubber) can pay the cost on demand while
  // canvas-only views skip it.
  loadEventHistoryIfMissing: (runId: string) => Promise<void>;
  // Optimistic header status flip — used after Resume/Cancel HTTP calls
  // so the UI doesn't wait for the corresponding event to arrive.
  setRunStatus: (status: RunHeader["status"]) => void;

  // Replaces the inbox slice from a REST hydration; merges with any
  // events that arrived in the meantime so live status overrides a
  // stale REST snapshot.
  setQueuedMessages: (messages: QueuedUserMessage[]) => void;

  setLogSubscribed: (subscribed: boolean) => void;
  applyLogChunk: (chunk: { offset: number; text: string; total?: number }) => void;
  markLogTerminated: () => void;
  clearLog: () => void;

  // Manual URL entry from the Browser pane URL bar. Cleared by `null`.
  // A manual URL takes precedence over workflow-emitted URLs until
  // explicitly cleared.
  setManualPreviewUrl: (url: string | null) => void;

  // Live-mode session toggle from the BrowserPane debug-attach
  // button. Pass `null` to drop back to viewer mode. The
  // browser_session_started / _ended event pair drives the same
  // field automatically when the runtime auto-attaches.
  setLiveSession: (info: BrowserSessionInfo | null) => void;

  reset: () => void;
}

const initialLogState: RunLogState = {
  start: 0,
  total: 0,
  text: "",
  subscribed: false,
  terminated: false,
};

const initialState = {
  runId: null,
  snapshot: null,
  events: [] as RunEvent[],
  // historyFetchedForRun marks which run's persisted event log has
  // already been hydrated via the REST /events endpoint. The WS no
  // longer auto-replays history (lazy mode), so the store tracks
  // whether it owes a hydration. Reset on reset() so a new run
  // navigation re-fetches.
  historyFetchedForRun: null as string | null,
  executionsById: new Map<string, ExecutionState>(),
  lastExecIDByNode: new Map<string, string>(),
  inFlightToolsByExec: new Map<string, InFlightTool[]>(),
  latestTodosByExec: new Map<string, TodoListSnapshot>(),
  pendingHumanInput: null as PendingHumanInput | null,
  queuedMessages: [] as QueuedUserMessage[],
  wsState: "idle" as WsState,
  followTail: true,
  log: initialLogState,
  browser: initialBrowserState,
  wsReconnectToken: 0,
};

export const useRunStore = create<RunStoreState>((set) => ({
  ...initialState,

  setRunId: (id) => set({ runId: id }),
  setWsState: (state) => set({ wsState: state }),
  setFollowTail: (follow) => set({ followTail: follow }),
  requestWsReconnect: () =>
    set((s) => ({ wsReconnectToken: s.wsReconnectToken + 1 })),

  applySnapshot: (snap) => {
    set((state) => {
      // Stale-snapshot guard: REST `getRun` and the WS "snapshot"
      // envelope are TWO concurrent sources of the same data. If the
      // REST round-trip is slower and resolves AFTER the WS already
      // pushed a newer snapshot (or after WS events advanced the
      // store past snap.last_seq), the older snapshot would regress
      // executionsById — leaving a finished node still showing
      // "running" because the snapshot at that moment had it
      // mid-flight. Detect the case via last_seq and bail.
      if (state.snapshot && state.snapshot.last_seq > snap.last_seq) {
        return state;
      }

      const map = new Map<string, ExecutionState>();
      for (const e of snap.executions) {
        map.set(e.execution_id, e);
      }
      // Rebuild lastExecIDByNode from the snapshot. `snap.executions`
      // is ordered by start time (backend's `b.order`) so the last
      // occurrence per (branch, node) is the most recently started —
      // exactly what node_finished and friends need to target.
      const lastExecIDByNode = new Map<string, string>();
      for (const e of snap.executions) {
        lastExecIDByNode.set(execKey(e.branch_id || "main", e.ir_node_id), e.execution_id);
      }
      const rehydrated = rehydratePendingHumanInput(snap);

      // Re-apply any events the WS already delivered that are NEWER
      // than the snapshot. Without this, those events were silently
      // dropped (the old filter `seq <= last_seq` discarded them) and
      // their state mutations were lost — the dominant root cause of
      // "two nodes show as running" UI glitches on initial mount.
      const newerEvents =
        state.events.length === 0
          ? []
          : state.events.filter((e) => e.seq > snap.last_seq);
      if (newerEvents.length === 0) {
        return {
          snapshot: snap,
          executionsById: map,
          lastExecIDByNode,
          // Snapshots don't carry "currently in-flight tool" state, so
          // drop any stale entries — the live event stream will
          // repopulate from the next tool_started onward.
          inFlightToolsByExec: new Map(),
          latestTodosByExec: new Map(),
          events:
            state.events.length === 0
              ? state.events
              : state.events.filter((e) => e.seq <= snap.last_seq),
          pendingHumanInput: rehydrated,
        };
      }
      // Reduce newer events against the snapshot's base. We pass an
      // empty events array so reduceEvents' lastSeq tracker starts
      // below newerEvents[0].seq and processes them all (the tracker
      // exists to drop out-of-order replays, not to gate fresh
      // application).
      const partial = reduceEvents(
        {
          events: [],
          executionsById: map,
          lastExecIDByNode,
          inFlightToolsByExec: new Map(),
          latestTodosByExec: new Map(),
          snapshot: snap,
          pendingHumanInput: rehydrated,
          browser: state.browser,
          queuedMessages: state.queuedMessages,
        },
        newerEvents,
      );
      return {
        ...partial,
        // Preserve the full event history (snapshot + post-snapshot)
        // so timeline scrubbing and edge rendering have the full
        // record.
        events: [...state.events.filter((e) => e.seq <= snap.last_seq), ...newerEvents],
      };
    });
  },

  applyEvent: (evt) => {
    set((state) => reduceEvents(state, [evt]));
  },

  applyEventsBatch: (evts) => {
    if (evts.length === 0) return;
    set((state) => reduceEvents(state, evts));
  },

  loadEventHistoryIfMissing: async (runId) => {
    const state = useRunStore.getState();
    if (state.historyFetchedForRun === runId) return;
    if (state.runId !== null && state.runId !== runId) return;
    // Coalesce concurrent callers (EventLog + Scrubber mounting in the
    // same render pass, or several mounts before `set()` lands) on a
    // single fetch. The optimistic marker below also helps, but there
    // is a micro-race between the getState() check above and the set()
    // call — multiple callers can pass both before either has stored
    // the marker, leading to duplicate `/events` GETs.
    const inflight = inflightHistoryFetches.get(runId);
    if (inflight) {
      await inflight;
      return;
    }
    set({ historyFetchedForRun: runId });
    const fetchPromise = (async () => {
      try {
        const fetched = await loadEvents(runId);
        if (useRunStore.getState().runId !== runId) return;
        useRunStore.getState().applyEventsBatch(fetched);
      } catch (err) {
        if (useRunStore.getState().historyFetchedForRun === runId) {
          set({ historyFetchedForRun: null });
        }
        throw err;
      } finally {
        inflightHistoryFetches.delete(runId);
      }
    })();
    inflightHistoryFetches.set(runId, fetchPromise);
    await fetchPromise;
  },

  setRunStatus: (status) => {
    set((state) => {
      if (!state.snapshot) return state;
      const current = state.snapshot.run.status;
      if (current === status) return state;
      // Flipping back to "running" implies a fresh execution pass: clear
      // the prior finished_at + error so the duration ticker (header)
      // and status banners don't carry stale data, and drop
      // pendingHumanInput so the answer panel unmounts immediately
      // after submit (don't wait for the run_resumed WS event).
      const next: Partial<RunStoreState> = {
        snapshot: {
          ...state.snapshot,
          run: {
            ...state.snapshot.run,
            status,
            ...(status === "running"
              ? { finished_at: undefined, error: undefined }
              : {}),
          },
        },
      };
      if (status === "running" && state.pendingHumanInput) {
        next.pendingHumanInput = null;
      }
      return next;
    });
  },

  setQueuedMessages: (messages) =>
    set((state) => {
      if (sameQueuedMessages(state.queuedMessages, messages)) {
        return state;
      }
      // REST hydration races with WS events. Existing live records
      // (already partly through the lifecycle) win on status fields
      // so a slow round-trip can't regress an in-flight delivery.
      if (state.queuedMessages.length === 0) {
        return { queuedMessages: messages };
      }
      const liveById = new Map(state.queuedMessages.map((m) => [m.id, m]));
      const merged: QueuedUserMessage[] = [];
      const seen = new Set<string>();
      for (const incoming of messages) {
        const live = liveById.get(incoming.id);
        merged.push(live ? mergeQueuedMessage(incoming, live) : incoming);
        seen.add(incoming.id);
      }
      for (const m of state.queuedMessages) {
        if (!seen.has(m.id)) merged.push(m);
      }
      merged.sort((a, b) => a.queued_at.localeCompare(b.queued_at));
      return { queuedMessages: merged };
    }),

  setManualPreviewUrl: (url) =>
    set((s) => ({
      browser: {
        ...s.browser,
        currentUrl: url,
        scope: "external",
        source: url ? "manual" : null,
        kind: undefined,
      },
    })),

  setLiveSession: (info) =>
    set((s) => ({ browser: { ...s.browser, liveSession: info } })),

  setLogSubscribed: (subscribed) =>
    set((s) => ({ log: { ...s.log, subscribed } })),

  applyLogChunk: (chunk) =>
    set((s) => {
      if (!chunk.text) return s;
      const { log } = s;
      const incomingEnd = chunk.offset + chunk.text.length;
      if (incomingEnd <= log.start) return s;

      const currentEnd = log.start + log.text.length;
      let appendText: string;
      if (chunk.offset >= currentEnd) {
        appendText = chunk.text;
      } else {
        const skip = currentEnd - chunk.offset;
        if (skip >= chunk.text.length) {
          if (chunk.total !== undefined && chunk.total > log.total) {
            return { log: { ...log, total: chunk.total } };
          }
          return s;
        }
        appendText = chunk.text.slice(skip);
      }

      let nextText = log.text + appendText;
      let nextStart = log.start;
      if (nextText.length > MAX_LOG_BYTES) {
        const drop = nextText.length - LOG_TRIM_TARGET;
        nextText = nextText.slice(drop);
        nextStart += drop;
      }

      const nextTotal =
        chunk.total !== undefined && chunk.total > incomingEnd
          ? chunk.total
          : Math.max(log.total, incomingEnd);

      return {
        log: {
          ...log,
          start: nextStart,
          text: nextText,
          total: nextTotal,
          terminated: false,
        },
      };
    }),

  markLogTerminated: () =>
    set((s) => ({ log: { ...s.log, terminated: true, subscribed: false } })),

  clearLog: () => set({ log: initialLogState }),

  reset: () =>
    set({
      ...initialState,
      executionsById: new Map(),
      lastExecIDByNode: new Map(),
      inFlightToolsByExec: new Map(),
      latestTodosByExec: new Map(),
      log: initialLogState,
    }),
}));

// ---------------------------------------------------------------------------
// Selectors
// ---------------------------------------------------------------------------

// selectRunningExecution picks the most-recently-started running
// execution. Drives the "follow live" mode; returns null when nothing
// is running. Ties broken by started_at so a fan-out feels intuitive.
export function selectRunningExecution(
  execs: Map<string, ExecutionState>,
): ExecutionState | null {
  let best: ExecutionState | null = null;
  for (const e of execs.values()) {
    if (e.status !== "running") continue;
    if (!best) {
      best = e;
      continue;
    }
    const a = best.started_at ?? "";
    const b = e.started_at ?? "";
    if (b > a) best = e;
  }
  return best;
}

// queuedMessageFromEvent unmarshals one user_message_* event's Data
// block into a QueuedUserMessage. Returns null when the payload is
// missing the mandatory id field.
function queuedMessageFromEvent(evt: RunEvent): QueuedUserMessage | null {
  const data = (evt.data ?? {}) as Record<string, unknown>;
  const id = typeof data.id === "string" ? data.id : "";
  if (!id) return null;
  const text = typeof data.text === "string" ? data.text : "";
  const statusRaw =
    typeof data.status === "string" ? data.status : eventTypeToStatus(evt.type);
  const status =
    statusRaw === "queued" ||
    statusRaw === "delivered" ||
    statusRaw === "consumed" ||
    statusRaw === "cancelled"
      ? (statusRaw as QueuedMessageStatus)
      : "queued";
  const queuedAt =
    typeof data.queued_at === "string" ? data.queued_at : evt.timestamp;
  return {
    id,
    text,
    queued_at: queuedAt,
    delivered_at: stringOrNull(data.delivered_at),
    consumed_at: stringOrNull(data.consumed_at),
    cancelled_at: stringOrNull(data.cancelled_at),
    status,
  };
}

function eventTypeToStatus(t: string): QueuedMessageStatus {
  switch (t) {
    case "user_message_delivered":
      return "delivered";
    case "user_message_consumed":
      return "consumed";
    case "user_message_cancelled":
      return "cancelled";
    default:
      return "queued";
  }
}

function stringOrNull(v: unknown): string | null {
  return typeof v === "string" ? v : null;
}

// sameQueuedMessages compares two inbox lists by (id, status, text).
// Used by setQueuedMessages to skip allocating a new slice when REST
// hydration produces the same view as what's already in the store —
// avoids re-rendering every chatbox subscriber on a no-op refresh.
function sameQueuedMessages(
  a: QueuedUserMessage[],
  b: QueuedUserMessage[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i]!;
    const y = b[i]!;
    if (x.id !== y.id || x.status !== y.status || x.text !== y.text) {
      return false;
    }
  }
  return true;
}

// mergeQueuedMessage folds an incoming record into an existing one,
// preferring non-null transition timestamps from EITHER side and the
// incoming status (the backend is the source of truth: it rejects
// cancel-after-deliver at the store layer, so a status moving
// "backwards" can only happen as out-of-order delivery, never as a
// real state regression).
function mergeQueuedMessage(
  existing: QueuedUserMessage,
  incoming: QueuedUserMessage,
): QueuedUserMessage {
  return {
    ...existing,
    ...incoming,
    text: incoming.text || existing.text,
    queued_at: existing.queued_at || incoming.queued_at,
    delivered_at: incoming.delivered_at ?? existing.delivered_at,
    consumed_at: incoming.consumed_at ?? existing.consumed_at,
    cancelled_at: incoming.cancelled_at ?? existing.cancelled_at,
  };
}

// rehydratePendingHumanInput rebuilds the panel state from
// RunHeader.checkpoint when the WS event stream isn't available
// (page reload mid-pause). Mirror of pkg/store.Checkpoint subset.
function rehydratePendingHumanInput(
  snap: RunSnapshot,
): PendingHumanInput | null {
  if (snap.run.status !== "paused_waiting_human") return null;
  const cp = snap.run.checkpoint;
  if (!cp || typeof cp !== "object") return null;
  // Narrow each field with runtime type checks before reading. The
  // server-side Checkpoint shape is opaque (any subset of fields may
  // be missing on legacy snapshots), so a blind cast left
  // PendingHumanInput populated with undefined when the payload
  // didn't match expectations — the panel then rendered with no
  // questions and no interaction_id.
  const obj = cp as Record<string, unknown>;
  const nodeID = typeof obj.node_id === "string" ? obj.node_id : undefined;
  const interactionID =
    typeof obj.interaction_id === "string" ? obj.interaction_id : undefined;
  const questions =
    obj.interaction_questions && typeof obj.interaction_questions === "object"
      ? (obj.interaction_questions as Record<string, unknown>)
      : {};
  return {
    interaction_id: interactionID,
    node_id: nodeID,
    questions,
  };
}

// ---------------------------------------------------------------------------
// Helpers (mirror of the Go reducer in pkg/runview/snapshot.go)
// ---------------------------------------------------------------------------

type ReduceInput = Pick<
  RunStoreState,
  | "events"
  | "executionsById"
  | "lastExecIDByNode"
  | "inFlightToolsByExec"
  | "latestTodosByExec"
  | "snapshot"
  | "pendingHumanInput"
  | "queuedMessages"
  | "browser"
>;

// execKey composes the (branch, node) lookup key used by
// lastExecIDByNode. The `\t` separator is forbidden in both branch
// ids and IR node ids so the encoding is unambiguous.
function execKey(branch: string, nodeID: string): string {
  return `${branch || "main"}\t${nodeID}`;
}

// reduceEvents applies a contiguous run of events in a single pass and
// returns a partial state diff for zustand. Splitting the per-event
// switch out of the store closure lets us batch live and replayed
// events alike — replay used to thrash O(N²) due to `applyEvent` setting
// state once per event.
function reduceEvents(
  state: ReduceInput,
  evts: RunEvent[],
): Partial<RunStoreState> {
  // Drop out-of-order events relative to what's already in store, but
  // keep processing the rest of the batch — each individual event
  // remains an idempotent step on top of the running state.
  const tail = state.events[state.events.length - 1];
  let lastSeq = tail?.seq ?? -1;
  // Seq of the most recent run_resumed across the historical event
  // stream plus this batch. The node_started monotonic guard uses
  // this to recognise a post-resume re-execution (existing terminal
  // exec_id last touched BEFORE the resume) and let it flip back to
  // "running" — without this the canvas locks on the pre-resume
  // terminal status forever, the long-standing "pipeline running
  // but no node currently running" bug operators kept reporting.
  // Pure WS-history replays carry no run_resumed past the original
  // and therefore keep the guard intact.
  let lastResumedSeq = -1;
  for (const e of state.events) {
    if (e.type === "run_resumed" && e.seq > lastResumedSeq) {
      lastResumedSeq = e.seq;
    }
  }

  let executionsById = state.executionsById;
  let lastExecIDByNode = state.lastExecIDByNode;
  let inFlightToolsByExec = state.inFlightToolsByExec;
  let latestTodosByExec = state.latestTodosByExec;
  let snapshot = state.snapshot;
  let pendingHumanInput = state.pendingHumanInput;
  let queuedMessages = state.queuedMessages;
  let queuedMutated = false;
  const ensureQueuedCopy = () => {
    if (!queuedMutated) {
      queuedMessages = queuedMessages.slice();
      queuedMutated = true;
    }
  };
  let browser = state.browser;
  // Clone the executions map only when the first mutation happens; if
  // the whole batch only contains pass-through event types we keep the
  // identity stable so React skips re-renders downstream.
  let execMutated = false;
  const ensureExecCopy = () => {
    if (!execMutated) {
      executionsById = new Map(executionsById);
      execMutated = true;
    }
  };
  // Same lazy-copy pattern for the lookup map: a batch that doesn't
  // include any node_started leaves the map identity untouched.
  let lastExecIDMutated = false;
  const ensureLastExecIDCopy = () => {
    if (!lastExecIDMutated) {
      lastExecIDByNode = new Map(lastExecIDByNode);
      lastExecIDMutated = true;
    }
  };
  // Same lazy-copy pattern for the in-flight map: pure-event batches
  // (no tool starts/completions) leave the map identity untouched.
  let inFlightMutated = false;
  const ensureInFlightCopy = () => {
    if (!inFlightMutated) {
      inFlightToolsByExec = new Map(inFlightToolsByExec);
      inFlightMutated = true;
    }
  };
  // Clear in-flight entries for an execution. Used on node_finished and
  // every run-termination case so a missed tool_called event can't
  // leave a phantom spinner on the canvas forever.
  const clearInFlightFor = (execId: string) => {
    if (!inFlightToolsByExec.has(execId)) return;
    ensureInFlightCopy();
    inFlightToolsByExec.delete(execId);
  };
  const clearAllInFlight = () => {
    if (inFlightToolsByExec.size === 0) return;
    ensureInFlightCopy();
    inFlightToolsByExec = new Map();
    inFlightMutated = true;
  };
  // Same lazy-copy pattern for the latest-todos map.
  let todosMutated = false;
  const ensureTodosCopy = () => {
    if (!todosMutated) {
      latestTodosByExec = new Map(latestTodosByExec);
      todosMutated = true;
    }
  };
  const clearTodosFor = (execId: string) => {
    if (!latestTodosByExec.has(execId)) return;
    ensureTodosCopy();
    latestTodosByExec.delete(execId);
  };
  const clearAllTodos = () => {
    if (latestTodosByExec.size === 0) return;
    ensureTodosCopy();
    latestTodosByExec = new Map();
  };

  let runStatusOverride: RunHeader["status"] | null = null;
  let runErrorOverride: string | null = null;

  // Accumulate appended events in a separate array so we only build the
  // final history slice once (capped at MAX_EVENTS).
  const appended: RunEvent[] = [];

  for (const evt of evts) {
    if (evt.seq <= lastSeq) continue;
    lastSeq = evt.seq;
    appended.push(evt);

    const branch = evt.branch_id || "main";
    switch (evt.type) {
      case "node_started": {
        if (!evt.node_id) break;
        // Prefer the runtime-supplied iteration (loop-counter semantics
        // — only bumps on actual loop-edge traversal). Falls back to
        // the local exec-count heuristic for events emitted before
        // this field existed. Without the backend value, a recovery
        // retry of the same node was being counted as a new iteration
        // and the per-(node, iter) log filter couldn't find its lines.
        const rawIter = evt.data?.iteration;
        const iter =
          typeof rawIter === "number"
            ? rawIter
            : nextIteration(executionsById, branch, evt.node_id);
        // Prefer iteration_path (encodes EVERY containing loop's counter)
        // for the exec_id when present — a single int collapses nested-
        // loop executions onto the same id and locks the canvas on the
        // first attempt's terminal status. The path is a stable string
        // emitted by the runtime; older events without it fall back to
        // the legacy int form transparently.
        const rawPath = evt.data?.iteration_path;
        const id =
          typeof rawPath === "string" && rawPath.length > 0
            ? `exec:${branch || "main"}:${evt.node_id}:${rawPath}`
            : makeExecutionId(branch, evt.node_id, iter);
        const kind = (evt.data?.kind as string) ?? undefined;
        ensureExecCopy();
        const existing = executionsById.get(id);
        // Monotonic status: a duplicate `node_started` (history replay
        // on WS reconnect, REST snapshot landing after the WS already
        // saw node_finished, runtime re-emitting the same iter for any
        // reason) must NEVER downgrade a terminal state back to
        // "running" — the original "finished node keeps showing as
        // running" glitch.
        //
        // EXCEPT after a run_resumed: the runtime re-executes the
        // failed checkpoint node with the SAME (branch, node, iter),
        // so the exec_id collides with the pre-resume terminal entry.
        // If we blindly preserve "finished" the canvas locks on the
        // pre-resume snapshot forever — the long-standing "pipeline
        // running but no node currently running" bug. Compare
        // existing.last_seq vs lastResumedSeq: when the terminal
        // entry was last touched BEFORE the most recent run_resumed
        // event, it is a pre-resume artefact and a node_started
        // arriving now is a fresh attempt that gets to flip status.
        // Collision triage at the same exec_id (mirror of
        // pkg/runview/snapshot.go::handleNodeStarted):
        //   1. existing non-terminal — same execution, refresh seq.
        //   2. existing terminal + post-resume — flip back to running
        //      with fresh started_at (lastResumedSeq rule).
        //   3. existing terminal + no resume between — WS-history
        //      replay or runtime re-emission inside the same attempt;
        //      preserve terminal status, just advance seq markers.
        // With iteration_path keyed exec_ids, two distinct executions
        // never share an id, so cases 1 and 3 are purely about replays
        // of the SAME attempt.
        const isTerminal =
          existing !== undefined &&
          (existing.status === "finished" ||
            existing.status === "failed" ||
            existing.status === "paused_waiting_human");
        const preResumeArtefact =
          isTerminal &&
          lastResumedSeq >= 0 &&
          existing!.last_seq < lastResumedSeq;
        if (isTerminal && !preResumeArtefact) {
          // Case 3 — monotonic guard preserves terminal status, only
          // seq markers advance so subscribers know we saw the event.
          executionsById.set(id, {
            ...existing!,
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
          // Stamp lastExecID even in the guard branch — every
          // node_started, including replayed duplicates, is by
          // definition the latest event for this (branch, node), so
          // downstream events should attribute to it.
          ensureLastExecIDCopy();
          lastExecIDByNode.set(execKey(branch, evt.node_id), id);
          break;
        }
        // Fresh execution OR post-resume re-run: issue a clean running
        // entry. On post-resume we keep first_seq anchored on the
        // original event (scrubber + log-window still find the historical
        // attempt) but reset started_at / finished_at / error so the
        // user-visible "running for Xs" timer restarts.
        const baseStartedAt = preResumeArtefact
          ? evt.timestamp
          : existing?.started_at ?? evt.timestamp;
        executionsById.set(id, {
          execution_id: id,
          ir_node_id: evt.node_id,
          branch_id: branch,
          loop_iteration: iter,
          status: "running",
          kind: existing?.kind ?? kind,
          started_at: baseStartedAt,
          current_event_seq: evt.seq,
          first_seq: existing?.first_seq ?? evt.seq,
          last_seq: evt.seq,
        });
        // Stamp the (branch, node) → exec_id pointer so downstream
        // events (node_finished, tool_*, artifact_written) attribute
        // to the right exec. Required by Option 3: nested-loop
        // exec_ids can share scalar loop_iteration so the legacy
        // max-iter scan inside currentExec is non-deterministic.
        ensureLastExecIDCopy();
        lastExecIDByNode.set(execKey(branch, evt.node_id), id);
        break;
      }
      case "node_finished": {
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (!exec) break;
        ensureExecCopy();
        executionsById.set(exec.execution_id, {
          ...exec,
          status: exec.status === "running" ? "finished" : exec.status,
          finished_at: evt.timestamp,
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        clearInFlightFor(exec.execution_id);
        clearTodosFor(exec.execution_id);
        break;
      }
      case "tool_started": {
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (!exec) break;
        const toolName = (evt.data?.tool as string) ?? "";
        const toolUseID = (evt.data?.tool_use_id as string) ?? "";
        ensureInFlightCopy();
        const prev = inFlightToolsByExec.get(exec.execution_id) ?? [];
        const next = prev.concat({
          toolName,
          toolUseID,
          // Prefer the event timestamp so the elapsed counter stays
          // accurate even if the WS replays older events on reconnect.
          startedAt: Date.parse(evt.timestamp) || Date.now(),
        });
        inFlightToolsByExec.set(exec.execution_id, next);
        // TodoWrite (claude_code) and todo_write (claw) carry the live
        // task list on `data.input`. Capture the latest payload per
        // execution so the side panel can render it without scanning
        // the whole events array on every selector call.
        if (TODO_LIST_TOOL_NAMES.has(toolName)) {
          const todos = extractTodosFromInput(evt.data?.input);
          if (todos && todos.length > 0) {
            ensureTodosCopy();
            latestTodosByExec.set(exec.execution_id, {
              todos,
              updatedAt: Date.parse(evt.timestamp) || Date.now(),
              source: toolName,
            });
          }
        }
        break;
      }
      case "tool_called":
      case "tool_error": {
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (!exec) break;
        const prev = inFlightToolsByExec.get(exec.execution_id);
        if (!prev || prev.length === 0) break;
        const toolUseID = (evt.data?.tool_use_id as string) ?? "";
        const toolName = (evt.data?.tool as string) ?? "";
        // Prefer matching by toolUseID (only claude_code paths carry
        // one); fall back to the oldest entry with the same toolName,
        // and as a last resort drop the oldest entry to avoid leaks.
        let dropIdx = -1;
        if (toolUseID) {
          dropIdx = prev.findIndex((t) => t.toolUseID === toolUseID);
        }
        if (dropIdx < 0 && toolName) {
          dropIdx = prev.findIndex((t) => t.toolName === toolName);
        }
        if (dropIdx < 0) dropIdx = 0;
        ensureInFlightCopy();
        const next = prev.slice(0, dropIdx).concat(prev.slice(dropIdx + 1));
        if (next.length === 0) {
          inFlightToolsByExec.delete(exec.execution_id);
        } else {
          inFlightToolsByExec.set(exec.execution_id, next);
        }
        break;
      }
      case "artifact_written": {
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (!exec) break;
        const v = numericVersion(evt.data?.version);
        ensureExecCopy();
        executionsById.set(exec.execution_id, {
          ...exec,
          last_artifact_version: v ?? exec.last_artifact_version,
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "human_input_requested": {
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (exec) {
          ensureExecCopy();
          executionsById.set(exec.execution_id, {
            ...exec,
            status: "paused_waiting_human",
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        pendingHumanInput = {
          interaction_id: evt.data?.interaction_id as string | undefined,
          node_id: evt.node_id,
          questions: evt.data?.questions as Record<string, unknown> | undefined,
          raw: evt,
        };
        break;
      }
      case "run_resumed": {
        // Update lastResumedSeq so subsequent node_started events in
        // this same batch recognise the post-resume re-execution
        // pattern (see comment on lastResumedSeq above).
        if (evt.seq > lastResumedSeq) {
          lastResumedSeq = evt.seq;
        }
        const last = lastPausedExec(executionsById);
        if (last) {
          ensureExecCopy();
          executionsById.set(last.execution_id, {
            ...last,
            status: "running",
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        pendingHumanInput = null;
        runStatusOverride = "running";
        break;
      }
      case "run_failed": {
        const errMsg = (evt.data?.error as string) ?? null;
        const exec = currentExec(executionsById, lastExecIDByNode, branch, evt.node_id);
        if (exec) {
          ensureExecCopy();
          executionsById.set(exec.execution_id, {
            ...exec,
            status: "failed",
            finished_at: exec.finished_at ?? evt.timestamp,
            error: errMsg ?? exec.error,
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        // Mirror server's closeInFlightExecs: any OTHER exec still
        // marked "running" when the run fails should also be closed.
        // Parallel-branch shapes leave siblings in flight that the
        // engine has stopped driving.
        closeInFlightOnRunTermination(
          executionsById,
          ensureExecCopy,
          "failed",
          evt.timestamp,
          evt.seq,
          errMsg ?? undefined,
        );
        clearAllInFlight();
        clearAllTodos();
        runStatusOverride = "failed_resumable";
        runErrorOverride = errMsg;
        break;
      }
      case "run_finished": {
        // Any execution still flagged "running" when the run terminates is,
        // by definition, no longer in flight — the engine has stopped
        // driving it. Mirrors `pkg/runview/snapshot.go::closeInFlightExecs`
        // (commit 97f7c1a) on the live-WS path: without this the canvas
        // keeps pulsing the spinner on whatever node was last running
        // even after `run_finished` arrived.
        closeInFlightOnRunTermination(
          executionsById,
          ensureExecCopy,
          "finished",
          evt.timestamp,
          evt.seq,
          undefined,
        );
        clearAllInFlight();
        clearAllTodos();
        runStatusOverride = "finished";
        break;
      }
      case "run_cancelled": {
        const reason = (evt.data?.reason as string) ?? "cancelled by user";
        closeInFlightOnRunTermination(
          executionsById,
          ensureExecCopy,
          "failed",
          evt.timestamp,
          evt.seq,
          reason,
        );
        clearAllInFlight();
        clearAllTodos();
        runStatusOverride = "cancelled";
        break;
      }
      case "run_paused":
        runStatusOverride = "paused_waiting_human";
        break;
      case "browser_session_started": {
        const sessionId = (evt.data?.session_id as string) ?? "";
        if (!sessionId) break;
        browser = {
          ...browser,
          liveSession: {
            sessionId,
            nodeId: evt.node_id,
            startedAt: evt.timestamp,
          },
          lastEventSeqSeen: evt.seq,
        };
        break;
      }
      case "browser_session_ended": {
        const sessionId = (evt.data?.session_id as string) ?? "";
        // Clear only if the ended session matches the active one;
        // otherwise a stale-old end event would clobber a fresh
        // session.
        if (browser.liveSession && browser.liveSession.sessionId === sessionId) {
          browser = { ...browser, liveSession: null };
        }
        break;
      }
      case "browser_screenshot": {
        const attachmentName = (evt.data?.attachment_name as string) ?? "";
        if (!attachmentName) break;
        const shot: BrowserScreenshot = {
          seq: evt.seq,
          attachmentName,
          url: evt.data?.url as string | undefined,
          nodeId: evt.node_id,
          toolCallId: evt.data?.tool_call_id as string | undefined,
        };
        // Append in seq order (events arrive monotonically; preserve
        // the invariant explicitly so a future late event doesn't
        // break binary search).
        const list = browser.screenshots;
        let next: BrowserScreenshot[];
        if (list.length === 0 || list[list.length - 1]!.seq <= shot.seq) {
          next = list.concat(shot);
        } else {
          next = list.slice();
          next.push(shot);
          next.sort((a, b) => a.seq - b.seq);
        }
        if (next.length > MAX_BROWSER_SCREENSHOTS) {
          next = next.slice(next.length - MAX_BROWSER_SCREENSHOTS);
        }
        browser = { ...browser, screenshots: next };
        break;
      }
      case "preview_url_available": {
        const url = (evt.data?.url as string) ?? "";
        if (!url) break;
        const rawScope = evt.data?.scope as string | undefined;
        const scope: PreviewScope = rawScope === "internal" ? "internal" : "external";
        const rawSource = evt.data?.source as string | undefined;
        // Manual URLs from setManualPreviewUrl take precedence: don't
        // overwrite an active manual selection with a workflow event.
        if (browser.source === "manual" && browser.currentUrl) {
          browser = { ...browser, lastEventSeqSeen: evt.seq };
          break;
        }
        browser = {
          ...browser,
          currentUrl: url,
          scope,
          source: rawSource === "tool-stdout" ? "tool-stdout" : "runtime",
          kind: evt.data?.kind as string | undefined,
          lastEventSeqSeen: evt.seq,
        };
        break;
      }
      case "user_message_queued":
      case "user_message_delivered":
      case "user_message_consumed":
      case "user_message_cancelled": {
        const incoming = queuedMessageFromEvent(evt);
        if (!incoming) break;
        ensureQueuedCopy();
        const idx = queuedMessages.findIndex((m) => m.id === incoming.id);
        if (idx === -1) {
          queuedMessages.push(incoming);
          queuedMessages.sort((a, b) =>
            a.queued_at.localeCompare(b.queued_at),
          );
        } else {
          queuedMessages[idx] = mergeQueuedMessage(
            queuedMessages[idx]!,
            incoming,
          );
        }
        break;
      }
      default:
        break;
    }
  }

  if (appended.length === 0) {
    // Nothing new — keep the store identity stable so subscribers don't
    // re-render. (Catches the all-stale-replay edge case.)
    return {};
  }

  // Build the events array in a single allocation, applying the
  // MAX_EVENTS cap once at the end of the batch.
  let events: RunEvent[];
  const total = state.events.length + appended.length;
  if (total <= MAX_EVENTS) {
    events = state.events.concat(appended);
  } else {
    const dropFromState = Math.min(state.events.length, total - MAX_EVENTS);
    const carry = state.events.slice(dropFromState);
    if (carry.length + appended.length > MAX_EVENTS) {
      // Batch alone overshoots the cap — keep only its tail.
      events = appended.slice(appended.length - MAX_EVENTS);
    } else {
      events = carry.concat(appended);
    }
  }

  const next: Partial<RunStoreState> = { events };

  if (snapshot) {
    const lastEvt = appended[appended.length - 1]!;
    const baseRun =
      runStatusOverride !== null
        ? {
            ...snapshot.run,
            status: runStatusOverride,
            error: runErrorOverride ?? snapshot.run.error,
          }
        : snapshot.run;
    if (execMutated) {
      const orderedIds = orderedExecutionIds(state.snapshot, executionsById);
      snapshot = {
        ...snapshot,
        run: baseRun,
        executions: orderedIds.map((id) => executionsById.get(id)!),
        last_seq: lastEvt.seq,
      };
      next.snapshot = snapshot;
      next.executionsById = executionsById;
    } else {
      next.snapshot = { ...snapshot, run: baseRun, last_seq: lastEvt.seq };
    }
  } else if (execMutated) {
    next.executionsById = executionsById;
  }

  if (pendingHumanInput !== state.pendingHumanInput) {
    next.pendingHumanInput = pendingHumanInput;
  }
  if (queuedMutated) {
    next.queuedMessages = queuedMessages;
  }
  if (browser !== state.browser) {
    next.browser = browser;
  }
  if (inFlightMutated) {
    next.inFlightToolsByExec = inFlightToolsByExec;
  }
  if (todosMutated) {
    next.latestTodosByExec = latestTodosByExec;
  }
  if (lastExecIDMutated) {
    next.lastExecIDByNode = lastExecIDByNode;
  }
  return next;
}

function makeExecutionId(branch: string, nodeId: string, iteration: number): string {
  return `exec:${branch || "main"}:${nodeId}:${iteration}`;
}

function nextIteration(
  execs: Map<string, ExecutionState>,
  branch: string,
  nodeId: string,
): number {
  let max = -1;
  for (const e of execs.values()) {
    if (e.branch_id === branch && e.ir_node_id === nodeId) {
      if (e.loop_iteration > max) max = e.loop_iteration;
    }
  }
  return max + 1;
}

// currentExec returns the exec that downstream events for (branch,
// node) should attribute to — i.e. the most recently started exec.
// Resolution order:
//   1. lastExecIDByNode (path-aware, populated by node_started for
//      every event the runtime emits) — mirror of the backend's
//      SnapshotBuilder.lastExecID.
//   2. legacy max(loop_iteration) scan for historical snapshots
//      pre-Option-3 where lastExecIDByNode wasn't populated yet.
//      Sufficient when each new exec strictly increments
//      loop_iteration, breaks down under nested loops where multiple
//      iteration_path execs can share the same scalar loop_iteration.
function currentExec(
  execs: Map<string, ExecutionState>,
  lastExecIDByNode: Map<string, string>,
  branch: string,
  nodeId: string | undefined,
): ExecutionState | null {
  if (!nodeId) return null;
  const recorded = lastExecIDByNode.get(execKey(branch, nodeId));
  if (recorded) {
    const e = execs.get(recorded);
    if (e) return e;
  }
  let best: ExecutionState | null = null;
  for (const e of execs.values()) {
    if (e.branch_id === branch && e.ir_node_id === nodeId) {
      if (!best || e.loop_iteration > best.loop_iteration) best = e;
    }
  }
  return best;
}

// closeInFlightOnRunTermination flips every still-"running" execution to
// a terminal status when the run itself terminates. Mirrors the
// server-side `pkg/runview/snapshot.go::closeInFlightExecs` so the live
// WebSocket path stays consistent with what the snapshot API returns.
function closeInFlightOnRunTermination(
  execs: Map<string, ExecutionState>,
  ensureCopy: () => void,
  finalStatus: ExecutionState["status"],
  timestamp: string,
  seq: number,
  errorReason: string | undefined,
): void {
  let mutated = false;
  for (const e of execs.values()) {
    if (e.status === "running") {
      if (!mutated) {
        ensureCopy();
        mutated = true;
      }
      execs.set(e.execution_id, {
        ...e,
        status: finalStatus,
        finished_at: e.finished_at ?? timestamp,
        error: errorReason ?? e.error,
        current_event_seq: seq,
        last_seq: seq,
      });
    }
  }
}

function lastPausedExec(execs: Map<string, ExecutionState>): ExecutionState | null {
  // Iterate insertion order (Map preserves it) and pick the latest paused.
  let last: ExecutionState | null = null;
  for (const e of execs.values()) {
    if (e.status === "paused_waiting_human") last = e;
  }
  return last;
}

function numericVersion(v: unknown): number | undefined {
  if (typeof v === "number") return Math.trunc(v);
  return undefined;
}

// orderedExecutionIds preserves the canvas order: existing snapshot
// executions stay in their place; new ones append at the end.
function orderedExecutionIds(
  prev: RunSnapshot | null,
  next: Map<string, ExecutionState>,
): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  if (prev) {
    for (const e of prev.executions) {
      if (next.has(e.execution_id)) {
        out.push(e.execution_id);
        seen.add(e.execution_id);
      }
    }
  }
  for (const id of next.keys()) {
    if (!seen.has(id)) out.push(id);
  }
  return out;
}

// selectInFlightTools returns every in-flight tool matching the given
// filter, ordered by `startedAt` ascending (oldest first). The Logs
// panel partitions this list into synchronous tools (which dominate
// the footer with "Running <tool>") and asynchronous ones (Agent/Task
// sub-agents — surfaced as a secondary count chip while the footer
// shows the random-words "thinking" loader).
//
// When filterNodeId is set we scope to (filterNodeId, filterIteration ??
// 0) — the same scoping the existing `active` selector uses — so a
// sibling node running in parallel can't leak a spinner into the
// per-node Logs tab.
//
// When filterNodeId is null we return entries across all executions;
// the global Logs panel is intentionally loud-and-coarse here,
// mirroring how the random-words footer already fires on any execution
// running anywhere.
export function selectInFlightTools(
  state: Pick<RunStoreState, "inFlightToolsByExec" | "executionsById">,
  filterNodeId: string | null = null,
  filterIteration: number | null = null,
): InFlightTool[] {
  if (state.inFlightToolsByExec.size === 0) return [];
  const out: InFlightTool[] = [];
  for (const [execId, tools] of state.inFlightToolsByExec) {
    if (tools.length === 0) continue;
    if (filterNodeId) {
      const exec = state.executionsById.get(execId);
      if (!exec) continue;
      if (exec.ir_node_id !== filterNodeId) continue;
      if (exec.loop_iteration !== (filterIteration ?? 0)) continue;
    }
    for (const t of tools) out.push(t);
  }
  out.sort((a, b) => a.startedAt - b.startedAt);
  return out;
}

// selectActiveTodos returns the most recently updated todo list snapshot
// for the given filter scope, or null when nothing has been emitted. The
// side panel uses this to render the live task list. Scoping mirrors
// `selectInFlightTools`: a node-filtered tab is scoped to (filterNodeId,
// filterIteration ?? 0); the global tab returns the most recent snapshot
// across all executions.
export function selectActiveTodos(
  state: Pick<
    RunStoreState,
    "latestTodosByExec" | "executionsById"
  >,
  filterNodeId: string | null = null,
  filterIteration: number | null = null,
): TodoListSnapshot | null {
  if (state.latestTodosByExec.size === 0) return null;
  let best: TodoListSnapshot | null = null;
  for (const [execId, snap] of state.latestTodosByExec) {
    if (filterNodeId) {
      const exec = state.executionsById.get(execId);
      if (!exec) continue;
      if (exec.ir_node_id !== filterNodeId) continue;
      if (exec.loop_iteration !== (filterIteration ?? 0)) continue;
    }
    if (!best || snap.updatedAt >= best.updatedAt) best = snap;
  }
  return best;
}

// selectPendingAgents returns the list of in-flight agentic tool calls
// (Agent/Task / agent/task) within the filter scope, oldest first. The
// side panel surfaces a count + the oldest elapsed time so the operator
// knows how many sub-agents are currently pending even though they're
// not visible in the footer.
export function selectPendingAgents(
  state: Pick<RunStoreState, "inFlightToolsByExec" | "executionsById">,
  filterNodeId: string | null = null,
  filterIteration: number | null = null,
): InFlightTool[] {
  const all = selectInFlightTools(state, filterNodeId, filterIteration);
  return all.filter((t) => AGENTIC_TOOL_NAMES.has(t.toolName));
}

// Re-export header type for component imports.
export type { RunHeader };
