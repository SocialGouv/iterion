import type { ExecutionState, RunEvent } from "@/api/runs";
import { stepIteration } from "./eventIter";

// buildExecutionsAt is a pure port of the store reducer's per-event
// folding logic. Given the full events array and a target seq, it
// returns the execution map state as it would have been after
// applying all events with seq <= targetSeq.
//
// Used by the time-travel scrubber: when the user drags the slider, we
// recompute a virtual snapshot client-side without bothering the
// backend. The backend reducer (pkg/runview/snapshot.go) is documented
// as deterministic precisely to enable this.
export function buildExecutionsAt(
  events: RunEvent[],
  targetSeq: number,
): ExecutionState[] {
  const execs = new Map<string, ExecutionState>();
  const counts = new Map<string, number>();
  // Seq of the most recent run_resumed seen during this fold. The
  // monotonic guard in the node_started case below refuses to flip
  // a terminal exec back to "running" on a duplicate node_started,
  // which is correct for WS-history replay and runtime re-emissions
  // inside the same execution attempt. It is wrong for a true
  // post-resume re-execution: the runtime re-runs the failed node
  // with the SAME (branch, node, iter) so the exec_id collides, and
  // the guard would otherwise lock the canvas on the pre-resume
  // terminal status. The seq comparison disambiguates: an existing
  // terminal exec whose last_seq predates the latest run_resumed is
  // a pre-resume artefact and the new node_started is a fresh attempt.
  let lastResumedSeq = -1;

  const currentExecFor = (
    branch: string,
    nodeId: string,
  ): ExecutionState | null => {
    let best: ExecutionState | null = null;
    for (const e of execs.values()) {
      if (e.branch_id === branch && e.ir_node_id === nodeId) {
        if (!best || e.loop_iteration > best.loop_iteration) best = e;
      }
    }
    return best;
  };

  for (const evt of events) {
    if (evt.seq > targetSeq) break;
    const branch = evt.branch_id || "main";

    switch (evt.type) {
      case "node_started": {
        if (!evt.node_id) break;
        const iter = stepIteration(counts, evt);
        const id = `exec:${branch}:${evt.node_id}:${iter}`;
        const kind = (evt.data?.["kind"] as string) ?? undefined;
        const existing = execs.get(id);
        // Monotonic status: mirror the live store reducer (see
        // store/run.ts) — a duplicate node_started for an exec id that
        // already reached a terminal state must not flip it back to
        // "running". The time-travel scrubber would otherwise lose
        // finished_at when replaying a stream that includes a recovery
        // retry that the runtime emitted as a fresh node_started.
        const isTerminal =
          existing &&
          (existing.status === "finished" ||
            existing.status === "failed" ||
            existing.status === "paused_waiting_human");
        const preResumeArtefact =
          isTerminal && lastResumedSeq >= 0 && existing!.last_seq < lastResumedSeq;
        if (isTerminal && !preResumeArtefact) {
          execs.set(id, {
            ...existing!,
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
          break;
        }
        // Fresh execution (no existing entry) OR post-resume re-run of a
        // previously-finished exec. In the post-resume case we keep
        // first_seq anchored on the original event (so the scrubber
        // still finds the historical log window) but issue a fresh
        // started_at and clear finished_at / error since this is a new
        // attempt the user wants to watch from now.
        const baseStartedAt = preResumeArtefact
          ? evt.timestamp
          : existing?.started_at ?? evt.timestamp;
        execs.set(id, {
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
        break;
      }
      case "node_finished": {
        const cur = currentExecFor(branch, evt.node_id ?? "");
        if (!cur) break;
        execs.set(cur.execution_id, {
          ...cur,
          status: cur.status === "running" ? "finished" : cur.status,
          finished_at: evt.timestamp,
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "artifact_written": {
        const cur = currentExecFor(branch, evt.node_id ?? "");
        if (!cur) break;
        const v =
          typeof evt.data?.["version"] === "number"
            ? Math.trunc(evt.data["version"] as number)
            : cur.last_artifact_version;
        execs.set(cur.execution_id, {
          ...cur,
          last_artifact_version: v,
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "human_input_requested": {
        const cur = currentExecFor(branch, evt.node_id ?? "");
        if (!cur) break;
        execs.set(cur.execution_id, {
          ...cur,
          status: "paused_waiting_human",
          current_event_seq: evt.seq,
          last_seq: evt.seq,
        });
        break;
      }
      case "run_resumed": {
        // Stash the resume seq so subsequent node_started events can
        // recognise a post-resume re-execution and override the
        // monotonic terminal guard. See lastResumedSeq above.
        lastResumedSeq = evt.seq;
        // Flip the latest paused execution back to running.
        let last: ExecutionState | null = null;
        for (const e of execs.values()) {
          if (e.status === "paused_waiting_human") last = e;
        }
        if (last) {
          execs.set(last.execution_id, {
            ...last,
            status: "running",
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        break;
      }
      case "run_failed": {
        const cur = currentExecFor(branch, evt.node_id ?? "");
        if (cur) {
          execs.set(cur.execution_id, {
            ...cur,
            status: "failed",
            finished_at: cur.finished_at ?? evt.timestamp,
            error: (evt.data?.["error"] as string) ?? cur.error,
            current_event_seq: evt.seq,
            last_seq: evt.seq,
          });
        }
        break;
      }
      default:
        break;
    }
  }

  return Array.from(execs.values());
}

// timelineMarks scans the events stream and returns the seq positions
// of run-level milestones (started, paused, resumed, finished, failed,
// cancelled). The Scrubber displays these as tick marks so the user
// can see "where things happened" without zooming in.
export interface TimelineMark {
  seq: number;
  type: string;
}

export function timelineMarks(events: RunEvent[]): TimelineMark[] {
  const interesting = new Set([
    "run_started",
    "run_paused",
    "run_resumed",
    "run_finished",
    "run_failed",
    "run_cancelled",
    "human_input_requested",
  ]);
  const out: TimelineMark[] = [];
  for (const e of events) {
    if (interesting.has(e.type)) out.push({ seq: e.seq, type: e.type });
  }
  return out;
}
