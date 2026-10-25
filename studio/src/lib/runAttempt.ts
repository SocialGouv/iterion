import type { ExecutionState, RunAttempt, RunEvent, RunSummary, WireWorkflow } from "@/api/runs";
import { buildExecutionsAt } from "./snapshotReducer";

// A boundary belongs to the attempt it opens. Sequence wins over timestamps,
// including two resumes inside one millisecond. Legacy streams still have
// boundaries even though their historical source cannot be reconstructed.
export function scopeAttempt(events: RunEvent[], attempts: RunAttempt[], selectedID: string | null) {
  const attempt = attempts.find((a) => a.id === selectedID) ?? attempts[attempts.length - 1];
  const index = attempt ? attempts.indexOf(attempt) : -1;
  const next = index >= 0 ? attempts[index + 1] : undefined;
  const boundaries = events.filter((e) => e.type === "run_started" || e.type === "run_resumed");
  const boundary = attempt
    ? boundaries.find((e) => (e.data?.attempt as RunAttempt | undefined)?.id === attempt.id)
    : boundaries[boundaries.length - 1];
  const following = boundary ? boundaries.find((e) => e.seq > boundary.seq) : undefined;
  const startSeq = boundary?.seq;
  const endSeq = following?.seq;
  const start = attempt?.started_at ?? boundary?.timestamp;
  const end = next?.started_at;
  const scoped = events.filter((e) =>
    (startSeq !== undefined ? e.seq >= startSeq : !start || compareInstants(e.timestamp, start) >= 0) &&
    (endSeq !== undefined ? e.seq < endSeq : !end || compareInstants(e.timestamp, end) < 0),
  );
  return { attempt, events: scoped, startSeq, endSeq, start, end,
    isCurrent: !next, key: attempt?.id ?? `legacy:${startSeq ?? -1}` };
}

export function attemptExecutions(scope: ReturnType<typeof scopeAttempt>, live: Map<string, ExecutionState>): Map<string, ExecutionState> {
  const folded = buildExecutionsAt(scope.events, Number.MAX_SAFE_INTEGER);
  const result = new Map(folded.map((e) => [e.execution_id, e]));
  if (scope.isCurrent) {
    for (const [id, e] of live) {
      const belongs = scope.startSeq !== undefined ? e.first_seq >= scope.startSeq :
        !scope.start || (e.started_at && compareInstants(e.started_at, scope.start) >= 0);
      if (belongs && (!result.has(id) || e.last_seq >= result.get(id)!.last_seq)) result.set(id, e);
    }
  }
  return result;
}

// Legacy or incomplete provenance must never hide an observed active node.
export function withExecutionNodes(wf: WireWorkflow, executions: ExecutionState[]): WireWorkflow {
  const ids = new Set(wf.nodes.map((n) => n.id));
  const missing = executions.filter((e) => (e.status === "running" || e.status === "paused_waiting_human") && !ids.has(e.ir_node_id));
  if (!missing.length) return wf;
  const nodes = [...wf.nodes];
  for (const e of missing) {
    if (ids.has(e.ir_node_id)) continue;
    ids.add(e.ir_node_id);
    nodes.push({ id: e.ir_node_id, kind: e.kind || "tool", description: "Observed in this attempt" });
  }
  return { ...wf, nodes };
}

// Child runs belong to the attempt that spawns them, not the newest child
// with a matching node ID. Explicit picks are reset with the attempt selector.
export function attemptChildren(children: RunSummary[] | undefined, scope: ReturnType<typeof scopeAttempt>, reattached: string[] = []): RunSummary[] {
  const attached = new Set(scope.events.filter((e) => e.type === "subbot_attached").map((e) => e.data?.child_run_id));
  return (children ?? []).filter((child) =>
    attached.has(child.id) ||
    (scope.isCurrent && reattached.includes(child.id)) ||
    ((!scope.start || compareInstants(child.created_at, scope.start) >= 0) &&
    (!scope.end || compareInstants(child.created_at, scope.end) < 0)),
  );
}

// Date.parse truncates RFC3339Nano at milliseconds. Keep the remaining six
// digits when two instants share that millisecond (Go timestamps retain them).
function compareInstants(left: string, right: string): number {
  const ms = Date.parse(left) - Date.parse(right);
  if (ms !== 0) return ms;
  const fraction = (s: string) => Number((s.match(/\.(\d+)(?:Z|[+-]\d\d:\d\d)$/)?.[1] ?? "").padEnd(9, "0").slice(3, 9));
  return fraction(left) - fraction(right);
}
