import { create } from "zustand";

import type {
  ExecutionState,
  RunEvent,
  RunSnapshot,
  RunHeader,
} from "@/api/runs";

// NoEventsSeq mirrors runview.NoEventsSeq on the Go side. Disambiguates
// "no events have been applied" from "the most recent event has seq 0".
export const NO_EVENTS_SEQ = -1;

// MAX_EVENTS caps in-memory history so a long-running run with thousands
// of events doesn't bloat the React store. Older events fall off the
// front; the scrubber (Phase 5) will refetch via /events?from=&to= when
// it needs them.
const MAX_EVENTS = 5000;

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
  questions?: Record<string, unknown>;
  raw: RunEvent;
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
  executionsById: Map<string, ExecutionState>;
  pendingHumanInput: PendingHumanInput | null;
  wsState: WsState;
  followTail: boolean;
  log: RunLogState;

  setRunId: (id: string | null) => void;
  setWsState: (state: WsState) => void;
  setFollowTail: (follow: boolean) => void;

  applySnapshot: (snap: RunSnapshot) => void;
  applyEvent: (evt: RunEvent) => void;
  applyEventsBatch: (evts: RunEvent[]) => void;
  // Optimistic header status flip — used after Resume/Cancel HTTP calls
  // so the UI doesn't wait for the corresponding event to arrive.
  setRunStatus: (status: RunHeader["status"]) => void;

  setLogSubscribed: (subscribed: boolean) => void;
  applyLogChunk: (chunk: { offset: number; text: string; total?: number }) => void;
  markLogTerminated: () => void;
  clearLog: () => void;

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
  executionsById: new Map<string, ExecutionState>(),
  pendingHumanInput: null as PendingHumanInput | null,
  wsState: "idle" as WsState,
  followTail: true,
  log: initialLogState,
};

export const useRunStore = create<RunStoreState>((set) => ({
  ...initialState,

  setRunId: (id) => set({ runId: id }),
  setWsState: (state) => set({ wsState: state }),
  setFollowTail: (follow) => set({ followTail: follow }),

  applySnapshot: (snap) => {
    const map = new Map<string, ExecutionState>();
    for (const e of snap.executions) {
      map.set(e.execution_id, e);
    }
    set((state) => {
      // Keep already-applied events that fall within the snapshot's
      // window. Wiping them broke edge rendering on revisit of finished
      // runs: the WS subscribe path computes from_seq from the snapshot
      // and never replays the historical tail. Preserving them lets the
      // hook detect "empty store" and trigger a full replay (from_seq=0)
      // while reconnect mid-run keeps incremental replay efficient.
      const trimmed = state.events.filter((e) => e.seq <= snap.last_seq);
      return {
        snapshot: snap,
        executionsById: map,
        events: trimmed,
        pendingHumanInput: null,
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

  setRunStatus: (status) => {
    set((state) => {
      if (!state.snapshot) return state;
      const current = state.snapshot.run.status;
      if (current === status) return state;
      // Flipping back to "running" implies a fresh execution pass: clear
      // the prior finished_at + error so the duration ticker (header)
      // and status banners don't carry stale data.
      const reset =
        status === "running"
          ? { finished_at: undefined, error: undefined }
          : null;
      return {
        snapshot: {
          ...state.snapshot,
          run: {
            ...state.snapshot.run,
            status,
            ...(reset ?? {}),
          },
        },
      };
    });
  },

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

  reset: () => set({ ...initialState, executionsById: new Map(), log: initialLogState }),
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

// ---------------------------------------------------------------------------
// Helpers (mirror of the Go reducer in pkg/runview/snapshot.go)
// ---------------------------------------------------------------------------

type ReduceInput = Pick<
  RunStoreState,
  "events" | "executionsById" | "snapshot" | "pendingHumanInput"
>;

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

  let executionsById = state.executionsById;
  let snapshot = state.snapshot;
  let pendingHumanInput = state.pendingHumanInput;
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
        const iter = nextIteration(executionsById, branch, evt.node_id);
        const id = makeExecutionId(branch, evt.node_id, iter);
        const kind = (evt.data?.kind as string) ?? undefined;
        ensureExecCopy();
        executionsById.set(id, {
          execution_id: id,
          ir_node_id: evt.node_id,
          branch_id: branch,
          loop_iteration: iter,
          status: "running",
          kind,
          started_at: evt.timestamp,
          current_event_seq: evt.seq,
          first_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "node_finished": {
        const exec = currentExec(executionsById, branch, evt.node_id);
        if (!exec) break;
        ensureExecCopy();
        executionsById.set(exec.execution_id, {
          ...exec,
          status: exec.status === "running" ? "finished" : exec.status,
          finished_at: evt.timestamp,
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "artifact_written": {
        const exec = currentExec(executionsById, branch, evt.node_id);
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
        const exec = currentExec(executionsById, branch, evt.node_id);
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
          questions: evt.data?.questions as Record<string, unknown> | undefined,
          raw: evt,
        };
        break;
      }
      case "run_resumed": {
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
        const exec = currentExec(executionsById, branch, evt.node_id);
        if (exec) {
          ensureExecCopy();
          executionsById.set(exec.execution_id, {
            ...exec,
            status: "failed",
            finished_at: exec.finished_at ?? evt.timestamp,
            error: (evt.data?.error as string) ?? exec.error,
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        runStatusOverride = "failed_resumable";
        runErrorOverride = (evt.data?.error as string) ?? null;
        break;
      }
      case "run_finished":
        runStatusOverride = "finished";
        break;
      case "run_cancelled":
        runStatusOverride = "cancelled";
        break;
      case "run_paused":
        runStatusOverride = "paused_waiting_human";
        break;
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

function currentExec(
  execs: Map<string, ExecutionState>,
  branch: string,
  nodeId: string | undefined,
): ExecutionState | null {
  if (!nodeId) return null;
  let best: ExecutionState | null = null;
  for (const e of execs.values()) {
    if (e.branch_id === branch && e.ir_node_id === nodeId) {
      if (!best || e.loop_iteration > best.loop_iteration) best = e;
    }
  }
  return best;
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

// Re-export header type for component imports.
export type { RunHeader };
