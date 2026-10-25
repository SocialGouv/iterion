import { useDeferredValue, useMemo } from "react";

import { type ExecutionState, type RunEvent } from "@/api/runs";
import { buildExecutionsAt } from "@/lib/snapshotReducer";
import { readNodeOutputMeta, type DelegateOutputMeta } from "@/lib/delegateMeta";

// `RuntimeLLMOverride` aliases the cross-file `DelegateOutputMeta`
// shape — kept under this name locally so its run-view role (override
// of the workflow-declared model/effort for the canvas "live" badge) is
// obvious at call sites.
type RuntimeLLMOverride = DelegateOutputMeta;

export interface DisplayedRunData {
  deferredScrubSeq: number | null;
  displayedExecutions: ExecutionState[];
  displayedEvents: RunEvent[];
  logClampBytes: number | null | undefined;
  runtimeOverrideByNode: Map<string, RuntimeLLMOverride>;
}

// Derives everything the canvas / detail / event log render from the
// current scrub position. When scrubbing, the run is shown *as it was* at
// deferredScrubSeq; when live (scrubSeq === null) the live data flows
// through untouched. All pure useMemo — no effects, no store writes — so
// this is a behaviour-preserving lift out of RunView.
export function useDisplayedRunData(
  scrubSeq: number | null,
  events: RunEvent[],
  liveExecutions: ExecutionState[],
): DisplayedRunData {
  // When scrubbing, derive a virtual snapshot at the chosen seq.
  // Otherwise use the live executions map.
  //
  // The range input on the Scrubber can fire onChange faster than 60 Hz
  // on drag, and `buildExecutionsAt` folds the full events array (up to
  // selected attempt) on each call. useDeferredValue lets React keep
  // the slider responsive while the heavier downstream computations
  // (executions snapshot, filtered events) catch up one frame behind.
  const deferredScrubSeq = useDeferredValue(scrubSeq);
  const displayedExecutions = useMemo(() => {
    if (deferredScrubSeq === null) return liveExecutions;
    return buildExecutionsAt(events, deferredScrubSeq);
  }, [deferredScrubSeq, events, liveExecutions]);

  const displayedEvents = useMemo(() => {
    if (deferredScrubSeq === null) return events;
    return events.filter((e) => e.seq <= deferredScrubSeq);
  }, [deferredScrubSeq, events]);

  // Absolute byte offset to clamp the bottom log panel during scrub /
  // replay: the backend stamps each event with the log buffer's byte
  // total at emission time (Event.log_offset). Take the latest event
  // with seq <= scrubSeq and use its offset. Falls back to undefined
  // for legacy runs whose events predate the feature — the log panel
  // then stays in live mode rather than going blank, which is the
  // less-bad degradation.
  const logClampBytes = useMemo<number | null | undefined>(() => {
    if (deferredScrubSeq === null) return null;
    if (events.length === 0) return 0;
    let lo = 0;
    let hi = events.length - 1;
    let best: number | undefined;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const evt = events[mid]!;
      if (evt.seq <= deferredScrubSeq) {
        best = evt.log_offset;
        lo = mid + 1;
      } else {
        hi = mid - 1;
      }
    }
    return best;
  }, [deferredScrubSeq, events]);

  // Fold llm_request and node_finished events into a per-node "what
  // actually ran" map. Latest event wins because seq is monotonic.
  // displayedEvents (not raw) so the time-travel scrubber rewinds too.
  // llm_request carries mid-flight overrides (claw); node_finished
  // carries the executor-stamped effective model + context window
  // (claude_code) via output._model / _context_*. See
  // pkg/backend/model/executor.go stampDelegateOutputMeta.
  const runtimeOverrideByNode = useMemo(() => {
    const m = new Map<string, RuntimeLLMOverride>();
    const update = (nodeID: string, patch: Partial<RuntimeLLMOverride>) => {
      const prev = m.get(nodeID) ?? {};
      m.set(nodeID, { ...prev, ...patch });
    };
    for (const e of displayedEvents) {
      if (!e.node_id) continue;
      const data = e.data ?? {};
      if (e.type === "llm_request") {
        const patch: Partial<RuntimeLLMOverride> = {};
        if (typeof data.model === "string") patch.model = data.model;
        if (typeof data.reasoning_effort === "string")
          patch.reasoning_effort = data.reasoning_effort;
        if (patch.model || patch.reasoning_effort) update(e.node_id, patch);
        continue;
      }
      if (e.type === "node_finished") {
        const patch = readNodeOutputMeta(
          data.output as Record<string, unknown> | undefined,
        );
        if (Object.keys(patch).length > 0) update(e.node_id, patch);
      }
    }
    return m;
  }, [displayedEvents]);

  return {
    deferredScrubSeq,
    displayedExecutions,
    displayedEvents,
    logClampBytes,
    runtimeOverrideByNode,
  };
}
