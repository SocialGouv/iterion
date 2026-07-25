import type { RunEvent } from "@/api/runs";

// iterationKey returns the (branch, node) compound key used by both
// the live store reducer and the time-travel snapshot reducer to
// track loop iteration counts. Keep them in sync — the backend's
// SnapshotBuilder uses the same convention.
function iterationKey(branch: string, nodeId: string): string {
  return `${branch || "main"} ${nodeId}`;
}

// stepIteration mutates the counts map and returns the iteration the
// given event belongs to. When node_started carries a backend-supplied
// `iteration` field (loop-counter semantics from the runtime), that
// value is used directly. Otherwise we fall back to the legacy
// exec-count heuristic where node_started increments by one. Non-started
// events return the most recent iteration recorded for their (branch,
// node) pair.
export function stepIteration(
  counts: Map<string, number>,
  evt: RunEvent,
): number {
  if (!evt.node_id) return 0;
  const branch = evt.branch_id || "main";
  const key = iterationKey(branch, evt.node_id);
  let cur = counts.get(key);
  if (cur === undefined) cur = -1;
  if (evt.type === "node_started") {
    const rawIter = evt.data?.iteration;
    if (typeof rawIter === "number") {
      cur = rawIter;
    } else {
      cur += 1;
    }
  }
  counts.set(key, cur);
  return cur < 0 ? 0 : cur;
}
