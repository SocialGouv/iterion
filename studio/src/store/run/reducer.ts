import type {
  ExecutionState,
  RunEvent,
  RunHeader,
  RunSnapshot,
  UserMessageEvent,
} from "@/api/runs";
import { isAsyncHumanInput } from "@/api/runs";
import { extractTodosFromInput } from "@/components/Runs/toolFormatters";
import { isRecord } from "@/lib/isRecord";

import type {
  BrowserScreenshot,
  PendingHumanInput,
  PreviewScope,
  QueuedMessageStatus,
  QueuedUserMessage,
  RunStoreState,
  TodoListSnapshot,
} from "../run";
import type { TodoItem } from "@/components/Runs/toolFormatters";

// MAX_EVENTS caps in-memory history so a long-running run with thousands
// of events doesn't bloat the React store. Older events fall off the
// front; the scrubber (Phase 5) will refetch via /events?from=&to= when
// it needs them.
const MAX_EVENTS = 5000;

// MAX_BROWSER_SCREENSHOTS bounds the in-memory history. The scrubber
// re-fetches via attachment URLs, so dropping older frames is safe;
// only the most-recent N stays available for scrub-without-network.
const MAX_BROWSER_SCREENSHOTS = 200;

// TODO_LIST_TOOL_NAMES groups tool names that emit a todo list payload.
// Claude Code's SDK uses CamelCase `TodoWrite`; claw exposes the same
// concept as `todo_write` (snake_case). Both write a `todos` array on
// the tool input that we surface in the side panel.
const TODO_LIST_TOOL_NAMES: ReadonlySet<string> = new Set([
  "TodoWrite",
  "todo_write",
]);

// queuedMessageFromEvent unmarshals one user_message_* event's Data
// block into a QueuedUserMessage. Returns null when the payload is
// missing the mandatory id field.
function queuedMessageFromEvent(evt: UserMessageEvent): QueuedUserMessage | null {
  const data = evt.data ?? {};
  const id = data.id ?? "";
  if (!id) return null;
  const statusRaw = data.status ?? eventTypeToStatus(evt.type);
  const status: QueuedMessageStatus =
    statusRaw === "queued" ||
    statusRaw === "delivered" ||
    statusRaw === "consumed" ||
    statusRaw === "cancelled"
      ? statusRaw
      : "queued";
  return {
    id,
    text: data.text ?? "",
    queued_at: data.queued_at ?? evt.timestamp,
    delivered_at: data.delivered_at ?? null,
    consumed_at: data.consumed_at ?? null,
    cancelled_at: data.cancelled_at ?? null,
    status,
  };
}

function eventTypeToStatus(t: UserMessageEvent["type"]): QueuedMessageStatus {
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

// sameQueuedMessages compares two inbox lists by (id, status, text).
// Used by setQueuedMessages to skip allocating a new slice when REST
// hydration produces the same view as what's already in the store —
// avoids re-rendering every chatbox subscriber on a no-op refresh.
export function sameQueuedMessages(
  a: QueuedUserMessage[],
  b: QueuedUserMessage[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
    if (!x || !y) return false;
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
export function mergeQueuedMessage(
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
export function rehydratePendingHumanInput(
  snap: RunSnapshot,
): PendingHumanInput | null {
  if (snap.run.status !== "paused_waiting_human") return null;
  const cp = snap.run.checkpoint;
  if (!cp) return null;
  // node_id / interaction_id are typed on RunCheckpoint; the questions
  // map sits behind the opaque index signature (any subset of fields
  // may be missing on legacy snapshots), so narrow it at runtime — a
  // blind cast left PendingHumanInput populated with undefined when
  // the payload didn't match expectations, and the panel rendered
  // with no questions and no interaction_id.
  const q = cp.interaction_questions;
  return {
    interaction_id: cp.interaction_id,
    node_id: cp.node_id,
    questions: isRecord(q) ? q : {},
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
  | "todoHistoryByExec"
  | "snapshot"
  | "pendingHumanInput"
  | "queuedMessages"
  | "browser"
>;

// execKey composes the (branch, node) lookup key used by
// lastExecIDByNode. The `\t` separator is forbidden in both branch
// ids and IR node ids so the encoding is unambiguous.
export function execKey(branch: string, nodeID: string): string {
  return `${branch || "main"}\t${nodeID}`;
}

// reduceEvents applies a contiguous run of events in a single pass and
// returns a partial state diff for zustand. Splitting the per-event
// switch out of the store closure lets us batch live and replayed
// events alike — replay used to thrash O(N²) due to `applyEvent` setting
// state once per event.
export function reduceEvents(
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
  let todoHistoryByExec = state.todoHistoryByExec;
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
  // Same lazy-copy pattern for the persistent task-list history map.
  // Deliberately NOT cleared on node_finished / run termination: the
  // Session board reads it to show the whole journey after the run ends.
  let todoHistoryMutated = false;
  const ensureTodoHistoryCopy = () => {
    if (!todoHistoryMutated) {
      todoHistoryByExec = new Map(todoHistoryByExec);
      todoHistoryMutated = true;
    }
  };
  const appendTodoHistory = (execId: string, snap: TodoListSnapshot) => {
    const prev = todoHistoryByExec.get(execId);
    // Collapse consecutive identical lists so each history entry is a
    // real change (the same TodoWrite payload can re-fire on retries).
    if (prev && prev.length > 0 && todosEqual(prev[prev.length - 1]!.todos, snap.todos)) {
      return;
    }
    ensureTodoHistoryCopy();
    todoHistoryByExec.set(execId, prev ? prev.concat(snap) : [snap]);
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
        const kind = evt.data?.kind;
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
          existing !== undefined &&
          lastResumedSeq >= 0 &&
          existing.last_seq < lastResumedSeq;
        if (isTerminal && existing !== undefined && !preResumeArtefact) {
          // Case 3 — monotonic guard preserves terminal status, only
          // seq markers advance so subscribers know we saw the event.
          executionsById.set(id, {
            ...existing,
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
        const toolName = evt.data?.tool ?? "";
        const toolUseID = evt.data?.tool_use_id ?? "";
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
            const snap = {
              todos,
              updatedAt: Date.parse(evt.timestamp) || Date.now(),
              source: toolName,
            };
            ensureTodosCopy();
            latestTodosByExec.set(exec.execution_id, snap);
            // Also append to the persistent, never-cleared history that
            // backs the Session board (Tasks tab).
            appendTodoHistory(exec.execution_id, snap);
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
        const toolUseID = evt.data?.tool_use_id ?? "";
        const toolName = evt.data?.tool ?? "";
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
        // Async question (ADR-081): the run keeps executing — no exec
        // pause, no blocking pause form. The conversation renders its
        // own non-blocking card from the event.
        if (isAsyncHumanInput(evt)) break;
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
          interaction_id: evt.data?.interaction_id,
          node_id: evt.node_id,
          questions: evt.data?.questions,
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
        const errMsg = evt.data?.error ?? null;
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
        const reason = evt.data?.reason ?? "cancelled by user";
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
      case "run_paused": {
        // The engine emits the same event type for every pause flavour.
        // Operator pause (POST /api/runs/:id/pause) tags reason=operator
        // and the daily spend cap tags reason=cost_cap_daily — both
        // persist as paused_operator on the backend. Human-input pause
        // leaves reason empty (or "human"). Branch so the live status
        // matches the persisted status without a second event round-trip.
        const reason = evt.data?.reason ?? "";
        runStatusOverride =
          reason === "operator" || reason === "cost_cap_daily"
            ? "paused_operator"
            : "paused_waiting_human";
        break;
      }
      case "browser_session_started": {
        const sessionId = evt.data?.session_id ?? "";
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
        const sessionId = evt.data?.session_id ?? "";
        // Clear only if the ended session matches the active one;
        // otherwise a stale-old end event would clobber a fresh
        // session.
        if (browser.liveSession && browser.liveSession.sessionId === sessionId) {
          browser = { ...browser, liveSession: null };
        }
        break;
      }
      case "browser_screenshot": {
        const attachmentName = evt.data?.attachment_name ?? "";
        if (!attachmentName) break;
        const shot: BrowserScreenshot = {
          seq: evt.seq,
          attachmentName,
          url: evt.data?.url,
          nodeId: evt.node_id,
          toolCallId: evt.data?.tool_call_id,
        };
        // Append in seq order (events arrive monotonically; preserve
        // the invariant explicitly so a future late event doesn't
        // break binary search).
        const list = browser.screenshots;
        const last = list[list.length - 1];
        let next: BrowserScreenshot[];
        if (last === undefined || last.seq <= shot.seq) {
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
        const url = evt.data?.url ?? "";
        if (!url) break;
        const rawScope = evt.data?.scope;
        const scope: PreviewScope = rawScope === "internal" ? "internal" : "external";
        const rawSource = evt.data?.source;
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
          kind: evt.data?.kind,
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
          const existingMsg = queuedMessages[idx];
          if (existingMsg !== undefined) {
            queuedMessages[idx] = mergeQueuedMessage(existingMsg, incoming);
          }
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
    const lastEvt = appended[appended.length - 1];
    if (lastEvt === undefined) return next;
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
        executions: orderedIds.flatMap((id) => {
          const e = executionsById.get(id);
          return e ? [e] : [];
        }),
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
  if (todoHistoryMutated) {
    next.todoHistoryByExec = todoHistoryByExec;
  }
  if (lastExecIDMutated) {
    next.lastExecIDByNode = lastExecIDByNode;
  }
  return next;
}

// todosEqual reports whether two task lists are identical (same items,
// same order, same status + visible text). Used to collapse consecutive
// duplicate TodoWrite payloads in the persistent history so each entry
// marks a real change.
function todosEqual(a: TodoItem[], b: TodoItem[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i]!;
    const y = b[i]!;
    if (
      x.content !== y.content ||
      x.status !== y.status ||
      x.activeForm !== y.activeForm
    ) {
      return false;
    }
  }
  return true;
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
