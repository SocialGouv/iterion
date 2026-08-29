import { createContext, useContext, type ReactNode, createElement } from "react";
import { create, useStore } from "zustand";

import {
  loadEvents,
  type ExecutionState,
  type RunEvent,
  type RunSnapshot,
  type RunHeader,
} from "@/api/runs";
import type { TodoItem } from "@/components/Runs/toolFormatters";

// NoEventsSeq mirrors runview.NoEventsSeq on the Go side. Disambiguates
// "no events have been applied" from "the most recent event has seq 0".
export const NO_EVENTS_SEQ = -1;

// inflightHistoryFetches keys runId → in-flight history fetch promise.
// Module-level (not inside the store) so callers that arrive while a
// fetch is in flight await the same promise instead of starting their
// own, even when they slip past the optimistic `historyFetchedForRun`
// marker due to React 18's concurrent rendering.
const inflightHistoryFetches = new Map<string, Promise<void>>();

import { MAX_LOG_BYTES, LOG_TRIM_TARGET, utf8Len, sliceFromByteOffset } from "./run/logBuffer";
import {
  execKey,
  mergeQueuedMessage,
  reduceEvents,
  rehydratePendingHumanInput,
  sameQueuedMessages,
} from "./run/reducer";

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
  // skill_refs is the list of bundle skill names attached to this
  // queued message. Mirrors store.QueuedUserMessage.SkillRefs on the
  // Go side. The AgentChatbox renders them as a chip suffix on the
  // message row so the operator can audit which skills were loaded.
  skill_refs?: string[];
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
// itself published or for content the studio controls).
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

const initialBrowserState: BrowserPaneState = {
  currentUrl: null,
  scope: "external",
  source: null,
  kind: undefined,
  lastEventSeqSeen: null,
  screenshots: [],
  liveSession: null,
};

// ResultLink is a run's headline deliverable — an opened pull request, a
// live deployment, a published app — surfaced as a prominent clickable
// link in the run summary (RunHeader's ResultLinksRow), like a CI run's
// "View deployment" button. Distinct from the embeddable Browser pane: a
// PR page blocks iframing, so it lives here as a link only. Derived purely
// from `preview_url_available` events whose `kind` is a result kind
// (RESULT_LINK_KINDS), so a reloaded terminal run reconstructs the list by
// replaying its event log — level-triggered, never live-only.
export interface ResultLink {
  url: string;
  // "pr" | "deploy" | "app" today, kept a plain string so a new
  // deterministic emitter can add a kind with no store change.
  kind: string;
  scope: PreviewScope;
  // Event seq that produced/last-classified this url. Stable identity +
  // sort key (ascending = discovery order).
  seq: number;
}

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

// TodoTimelineEntry groups one execution's full task-list history for the
// Session board (Tasks tab). Unlike `latestTodosByExec` — which the live
// Logs side panel reads and which the reducer clears on node_finished /
// run termination — the underlying `todoHistoryByExec` is never cleared,
// so the board survives run completion and shows the journey, not just
// the live snapshot. `exec` is the joined ExecutionState (for the
// friendly label, kind, status); it may be undefined for a cold-loaded
// run whose node_started hasn't been folded yet (rare; defensive).
export interface TodoTimelineEntry {
  execId: string;
  // Joined ExecutionState — the source for the friendly node label, kind,
  // status and start time. Undefined only defensively (a cold-loaded run
  // whose node_started hasn't folded yet).
  exec?: ExecutionState;
  // Ordered task-list snapshots (oldest → newest); consecutive identical
  // lists are collapsed by the reducer so each entry is a real change.
  snapshots: TodoListSnapshot[];
  latest: TodoListSnapshot;
}

export interface RunLogState {
  // start is the byte offset in the run's logical log stream where
  // text begins. start > 0 means the older bytes were evicted.
  start: number;
  // nextByte is the byte offset just PAST the last byte currently in
  // `text` — i.e. the exact `from_offset` to resume from. It is tracked
  // separately from `start + text.length` because `text.length` counts
  // UTF-16 code units, NOT bytes, while the backend keys every log
  // offset in bytes. The run console is dense with multi-byte glyphs
  // (ℹ️ = 6 bytes / 2 units, 🔧 = 4 / 2, ▸ = 3 / 1), so a code-unit
  // cursor drifts below the true byte position; feeding it back as
  // from_offset made the server resend — and the client re-append —
  // overlapping tails (the "nt -T /hom…" / doubled-line corruption).
  nextByte: number;
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
  // Evicted-head cache for the replay scrubber: the log bytes [0, until)
  // that rolled out of the in-memory window (start > 0), fetched on
  // demand from GET /runs/{id}/log?from=0&until=<start>. Only valid
  // while until === start — a further trim widens the gap and the view
  // refetches. Null until the scrubber first needs it.
  prefix: { until: number; text: string } | null;
}

export interface RunStoreState {
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
  // Full task-list history per execution (ordered, deduped), populated
  // on the SAME TodoWrite/todo_write tool_started events but NEVER
  // cleared on node_finished / run termination. Backs the persistent
  // Session board (Tasks tab) which must survive run completion and
  // show the journey across all nodes. For a finished run loaded cold
  // this is rebuilt by folding the persisted /events log (see
  // loadEventHistoryIfMissing) through the reducer.
  todoHistoryByExec: Map<string, TodoListSnapshot[]>;
  pendingHumanInput: PendingHumanInput | null;
  // Inbox of operator-queued chat messages for the run. Hydrated via
  // the REST GET /api/runs/{id}/queue-messages endpoint and live-
  // updated through the user_message_* event family.
  queuedMessages: QueuedUserMessage[];
  // Composer draft for the AgentChatbox. Lifted out of component-local
  // state so the WhatsNextView swap between AgentChatbox and the
  // HumanChatTurn footer (when the bot asks a question mid-typing)
  // doesn't unmount the textarea and discard the operator's text.
  chatDraft: string;
  wsState: WsState;
  followTail: boolean;
  log: RunLogState;
  browser: BrowserPaneState;
  // Headline result-links (opened PR, live deploy, …), deduped by url,
  // in discovery order. Event-derived (see ResultLink); rebuilt on reload
  // from the replayed event log alongside `browser`.
  resultLinks: ResultLink[];
  // Increments to request a fresh WS dial. The broker drops a run's
  // subscribers on terminal status (pkg/runview/service.go: CloseRun),
  // so after Resume the still-open WS conn no longer receives events
  // — bumping this token tears down the hook's effect and re-anchors
  // a new subscription against the now-active run.
  wsReconnectToken: number;

  // UI-event bus: child components that need to ask the RunView shell
  // to expand a collapsed panel post a request here, RunView watches
  // the token and reacts. Token-based (not a boolean) so consecutive
  // requests refire even if the bottom drawer is already open and the
  // user re-collapsed it manually between requests.
  uiOpenEventLogToken: number;

  setRunId: (id: string | null) => void;
  setWsState: (state: WsState) => void;
  setFollowTail: (follow: boolean) => void;
  requestWsReconnect: () => void;
  // Fired by ConversationEmptyState's "Show event log" link when the
  // run has been running > 30s without producing chat-renderable
  // output. Increments uiOpenEventLogToken so RunView can expand the
  // bottom drawer and switch its tab to "events".
  requestOpenEventLog: () => void;

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
  // resyncEventsAfterResume re-pulls the event log a couple of times,
  // detached from any component lifecycle, after a Resume. When a resume
  // re-pauses almost immediately (a human-only flow with no LLM between
  // gates) the WS reconnect can race the broker's subscriber-drop and miss
  // the next human_input_requested event, so the next gate's form never
  // folds into the conversation. Scheduling the refetch here — not in the
  // submitting form's setTimeout, which is cleared when that gate flips to
  // answered and the form unmounts — guarantees it fires. applyEventsBatch
  // dedupes by seq, so the repeated pulls only append the missed tail.
  resyncEventsAfterResume: (runId: string) => void;
  // Optimistic header status flip — used after Resume/Cancel HTTP calls
  // so the UI doesn't wait for the corresponding event to arrive.
  setRunStatus: (status: RunHeader["status"]) => void;

  // Replaces the inbox slice from a REST hydration; merges with any
  // events that arrived in the meantime so live status overrides a
  // stale REST snapshot.
  setQueuedMessages: (messages: QueuedUserMessage[]) => void;

  setChatDraft: (draft: string) => void;

  setLogSubscribed: (subscribed: boolean) => void;
  applyLogChunk: (chunk: { offset: number; text: string; total?: number }) => void;
  markLogTerminated: () => void;
  // Installs the evicted-head cache fetched by the replay scrubber (see
  // RunLogState.prefix). Pass null to drop it.
  setLogPrefix: (prefix: { until: number; text: string } | null) => void;
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
  nextByte: 0,
  total: 0,
  text: "",
  subscribed: false,
  terminated: false,
  prefix: null,
};

// freshInitial returns a value-only snapshot of the initial reducer
// state. Called per store instance so each parallel run tab gets its
// own Maps / arrays (previous module-level `initialState` shared the
// same Map identities across stores, which broke once we introduced
// multiple stores).
function freshInitial() {
  return {
    runId: null as string | null,
    snapshot: null as RunSnapshot | null,
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
    todoHistoryByExec: new Map<string, TodoListSnapshot[]>(),
    pendingHumanInput: null as PendingHumanInput | null,
    queuedMessages: [] as QueuedUserMessage[],
    chatDraft: "",
    wsState: "idle" as WsState,
    followTail: true,
    log: initialLogState,
    browser: initialBrowserState,
    resultLinks: [] as ResultLink[],
    wsReconnectToken: 0,
    uiOpenEventLogToken: 0,
  };
}

// createRunStore builds a fresh Zustand store with the reducer logic
// previously hosted on the module-level singleton. Each parallel run
// tab owns its own store instance; closing the tab disposes the store
// (see registry below). The legacy `useRunStore` is reconstructed by
// binding a default store + a Context-aware hook so existing call
// sites keep working unchanged.
export function createRunStore() {
  return create<RunStoreState>((set, get) => ({
    ...freshInitial(),

    setRunId: (id) => set({ runId: id }),
  setWsState: (state) => set({ wsState: state }),
  setFollowTail: (follow) => set({ followTail: follow }),
  requestWsReconnect: () =>
    set((s) => ({ wsReconnectToken: s.wsReconnectToken + 1 })),
  requestOpenEventLog: () =>
    set((s) => ({ uiOpenEventLogToken: s.uiOpenEventLogToken + 1 })),

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
          // Preserve the accumulated task-list history across a snapshot
          // (a snapshot never carries it); newer events append to it.
          todoHistoryByExec: state.todoHistoryByExec,
          snapshot: snap,
          pendingHumanInput: rehydrated,
          browser: state.browser,
          resultLinks: state.resultLinks,
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

  resyncEventsAfterResume: (runId) => {
    const refetch = () => {
      const state = get();
      if (state.runId !== runId) return;
      // Incremental tail fetch: only events newer than what the store
      // already holds (loadEvents supports ?from), not the whole log.
      const tail = state.events[state.events.length - 1];
      const fromSeq = tail ? tail.seq + 1 : 0;
      loadEvents(runId, fromSeq)
        .then((evts) => {
          if (get().runId === runId) get().applyEventsBatch(evts);
        })
        .catch(() => {});
    };
    // Two detached pulls cover the fast window (WS reconnected but the
    // broker already missed the event) and the slow one (resume→re-pause
    // is itself slow) without a poll loop; reduceEvents dedupes by seq so
    // the overlap is free. This is a client-side compensation for the
    // broker dropping a run's subscribers on every pause — the deeper fix
    // is to keep subscribers across a pause (pkg/runview broker lifecycle).
    const delaysMs = [400, 1300];
    delaysMs.forEach((ms) => setTimeout(refetch, ms));
  },

  loadEventHistoryIfMissing: async (runId) => {
    const state = get();
    if (state.historyFetchedForRun === runId) return;
    if (state.runId !== null && state.runId !== runId) return;
    // Coalesce concurrent callers (EventLog + Scrubber mounting in the
    // same render pass, or several mounts before `set()` lands) on a
    // single fetch. The optimistic marker below also helps, but there
    // is a micro-race between the get() check above and the set()
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
        if (get().runId !== runId) return;
        get().applyEventsBatch(fetched);
      } catch (err) {
        if (get().historyFetchedForRun === runId) {
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

  setChatDraft: (draft) =>
    set((state) => (state.chatDraft === draft ? state : { chatDraft: draft })),

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
      // Everything here is reasoned in BYTES — start, nextByte, total and
      // chunk.offset are all byte offsets in the backend's byte-keyed stream.
      // The buffer currently covers [log.start, log.nextByte). The chunk's
      // string is sliced via sliceFromByteOffset so byte overlaps convert to
      // the right UTF-16 code-unit cut (mixing bytes with String.length here
      // was what duplicated/dropped log segments after a WS reconnect resent
      // an overlapping tail of multi-byte glyphs).
      const chunkBytes = utf8Len(chunk.text);
      const incomingEndByte = chunk.offset + chunkBytes;

      // A total bump we should always propagate even when nothing is appended.
      const bumpTotal = (over: number) =>
        chunk.total !== undefined && chunk.total > over ? chunk.total : Math.max(log.total, over);

      // Chunk ends at or before what we already retain: pure resend / stale.
      if (incomingEndByte <= log.nextByte) {
        const t = bumpTotal(incomingEndByte);
        return t !== log.total ? { log: { ...log, total: t } } : s;
      }

      let appendText: string;
      if (chunk.offset >= log.nextByte) {
        // Contiguous with (or, defensively, ahead of) our tail — append whole.
        appendText = chunk.text;
      } else {
        // Partial overlap: drop the leading bytes we already have.
        appendText = sliceFromByteOffset(chunk.text, log.nextByte - chunk.offset);
      }
      if (!appendText) {
        const t = bumpTotal(incomingEndByte);
        return t !== log.total ? { log: { ...log, total: t } } : s;
      }

      let nextText = log.text + appendText;
      let nextStart = log.start;
      const nextByte = Math.max(log.nextByte, incomingEndByte);
      // Trim the retained window by BYTES (both constants are byte counts).
      const totalBytes = utf8Len(nextText);
      if (totalBytes > MAX_LOG_BYTES) {
        const trimmed = sliceFromByteOffset(nextText, totalBytes - LOG_TRIM_TARGET);
        nextStart += totalBytes - utf8Len(trimmed);
        nextText = trimmed;
      }

      return {
        log: {
          ...log,
          start: nextStart,
          nextByte,
          text: nextText,
          total: bumpTotal(nextByte),
          terminated: false,
        },
      };
    }),

  markLogTerminated: () =>
    set((s) => ({ log: { ...s.log, terminated: true, subscribed: false } })),

  setLogPrefix: (prefix) => set((s) => ({ log: { ...s.log, prefix } })),

  clearLog: () => set({ log: initialLogState }),

  reset: () =>
    set({
      ...freshInitial(),
    }),
  }));
}

export type RunStore = ReturnType<typeof createRunStore>;

// Default store used by call sites that don't have a RunStoreProvider
// in their React tree (App-level hooks, the WhatsNext session, etc.).
// Per-run tab stores are created via the registry (see getOrCreateRunStore)
// and mounted via RunStoreProvider; their hooks override this default.
const defaultRunStore = createRunStore();

const RunStoreContext = createContext<RunStore | null>(null);

interface RunStoreProviderProps {
  store: RunStore;
  children: ReactNode;
}

export function RunStoreProvider({ store, children }: RunStoreProviderProps) {
  return createElement(RunStoreContext.Provider, { value: store }, children);
}

// useRunStoreInstance returns the active RunStore (falling back to the
// module default when no Provider is mounted). Use sparingly — most
// React-side consumers should reach for useRunStore(selector) which
// memoises subscriptions automatically.
export function useRunStoreInstance(): RunStore {
  return useContext(RunStoreContext) ?? defaultRunStore;
}

// useRunStore preserves the (s) => x selector API from the singleton era
// so components don't need to be rewritten. Inside a RunStoreProvider it
// reads from that provider's store; outside, it falls back to the
// module-level default store — equivalent to the old singleton behavior.
export function useRunStore<T>(selector: (state: RunStoreState) => T): T {
  return useStore(useRunStoreInstance(), selector);
}

// getDefaultRunStore hands back the module default explicitly. Needed by
// scopes that mount their OWN RunStoreProvider around a subtree but must
// hand the default back to the subtree below it (the assistant dock,
// which runs a long-lived session in an isolated store while the rest of
// the app keeps reading the default one).
export function getDefaultRunStore(): RunStore {
  return defaultRunStore;
}

// Imperative façade for non-React callers (e.g. effects that need to
// poke the store without subscribing). Pre-tab callers used
// `useRunStore.getState()` — that pattern is preserved via this helper
// wrapping the default store.
export const runStore = {
  getState: () => defaultRunStore.getState(),
  setState: defaultRunStore.setState,
  subscribe: defaultRunStore.subscribe,
};

// Registry of stores keyed by runId. Populated lazily by
// getOrCreateRunStore as run tabs hydrate; cleared by disposeRunStore
// when a run tab closes. BackgroundRunSubscribers and RunTabHost are
// the primary consumers (Phase 2b wiring); App-level fallbacks still
// hit the module default above.
const REGISTRY = new Map<string, RunStore>();

export function getOrCreateRunStore(runId: string): RunStore {
  let store = REGISTRY.get(runId);
  if (!store) {
    store = createRunStore();
    store.getState().setRunId(runId);
    REGISTRY.set(runId, store);
  }
  return store;
}

export function disposeRunStore(runId: string): void {
  REGISTRY.delete(runId);
}

export function getRegisteredRunIds(): string[] {
  return Array.from(REGISTRY.keys());
}

// resetAllRunStores wipes every run store the studio is tracking. Used
// by project-switch listeners (the new project's runs are in a
// different store dir, so previous data is meaningless).
export function resetAllRunStores(): void {
  defaultRunStore.getState().reset();
  for (const store of REGISTRY.values()) {
    store.getState().reset();
  }
}

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

// selectTodoTimeline returns one entry per execution that ever emitted a
// task list, ordered chronologically (oldest first) by the execution's
// start time. Backs the Session board (Tasks tab): the last entry is the
// most recent ("Now"), earlier entries are the journey. Reads
// `todoHistoryByExec` (never cleared) — NOT `latestTodosByExec` (live
// only) — so it stays populated after the run finishes.
//
// Memoised on the (todoHistoryByExec, executionsById) map identities: the
// reducer swaps those references only when they actually change, so the
// selector — read via useShallow on every store update — returns a stable
// array (no re-allocation, no re-sort, no spurious re-render) for the far
// more frequent unrelated updates (log chunks, browser frames, chat). The
// cache is a single slot shared across run-store instances; that only
// costs a recompute when the active tab switches, and stays correct
// because the keys are compared by reference.
const EMPTY_TIMELINE: TodoTimelineEntry[] = [];
let timelineCacheHistory: RunStoreState["todoHistoryByExec"] | null = null;
let timelineCacheExecs: RunStoreState["executionsById"] | null = null;
let timelineCacheResult: TodoTimelineEntry[] = EMPTY_TIMELINE;

export function selectTodoTimeline(
  state: Pick<RunStoreState, "todoHistoryByExec" | "executionsById">,
): TodoTimelineEntry[] {
  if (
    timelineCacheHistory === state.todoHistoryByExec &&
    timelineCacheExecs === state.executionsById
  ) {
    return timelineCacheResult;
  }
  timelineCacheHistory = state.todoHistoryByExec;
  timelineCacheExecs = state.executionsById;

  if (state.todoHistoryByExec.size === 0) {
    timelineCacheResult = EMPTY_TIMELINE;
    return timelineCacheResult;
  }
  const out: TodoTimelineEntry[] = [];
  for (const [execId, snapshots] of state.todoHistoryByExec) {
    if (snapshots.length === 0) continue;
    out.push({
      execId,
      exec: state.executionsById.get(execId),
      snapshots,
      latest: snapshots[snapshots.length - 1]!,
    });
  }
  out.sort((a, b) => timelineOrderKey(a) - timelineOrderKey(b));
  timelineCacheResult = out;
  return timelineCacheResult;
}

// timelineOrderKey is the chronological sort key for a timeline entry:
// the execution's start time when known, else the first snapshot's
// update time (cold-loaded run whose node_started hasn't folded yet).
function timelineOrderKey(e: TodoTimelineEntry): number {
  if (e.exec?.started_at) {
    return Date.parse(e.exec.started_at) || e.snapshots[0]!.updatedAt;
  }
  return e.snapshots[0]!.updatedAt;
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
