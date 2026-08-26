import { useCallback, useMemo } from "react";
import { useQueries, type UseQueryResult } from "@tanstack/react-query";

import {
  getRun,
  getRunChildren,
  getRunWorkflow,
  type ExecutionState,
  type RunSnapshot,
  type RunSummary,
  type WireWorkflow,
} from "@/api/runs";
import {
  groupChildrenByNode,
  isSettledRunStatus,
  resolveSelectedSubbotChild,
} from "@/lib/subRuns";
import { makeSubbotChildId } from "@/lib/subbotGraph";

// Data feeds for the run canvas's INLINE subbot expansion
// (lib/subbotRunGraph) — RECURSIVE: each expanded frame displays ONE
// selected child run; when that child's workflow itself contains subbot
// nodes, the selected child's own children are fetched and grouped so
// the nested frames (`stage::step`) expand too. Three fixed levels
// (matching lib/subbotGraph.MAX_SUBBOT_EXPANSION_DEPTH) keep the hook
// order static; deeper subbots stay compact.
//
// Per level and frame: the child workflow shape (from the first child —
// all children of one node share the source), the SELECTED child's
// children (nested grouping + tab dots), and the SELECTED child's live
// executions (REST-polled at 3s while unsettled; siblings poll nothing
// until their tab is picked).
const CHILD_POLL_MS = 3000;
// Safety cap on frames tracked per level — beyond this the inline
// frames still render for the first N, later ones stay compact.
const MAX_FRAMES_PER_LEVEL = 16;

export interface InlineSubbotData {
  // frameId (expanded node id) -> child runs, across all levels.
  subRunsByNode: Map<string, RunSummary[]>;
  // frameId -> the child workflow its frame renders.
  childWorkflowsByNode: Map<string, WireWorkflow>;
  // child run id -> its live executions (selected children only).
  childExecutionsByRun: Map<string, ExecutionState[]>;
  // frameId -> the child run the frame displays (user pick, else the
  // live/latest child — see resolveSelectedSubbotChild).
  selectedChildByFrame: Map<string, string>;
}

const EMPTY: InlineSubbotData = {
  subRunsByNode: new Map(),
  childWorkflowsByNode: new Map(),
  childExecutionsByRun: new Map(),
  selectedChildByFrame: new Map(),
};

interface FrameSpec {
  frameId: string;
  children: RunSummary[];
}

interface LevelData {
  wfByFrame: Map<string, WireWorkflow>;
  selected: Map<string, string>;
  childrenOfSelected: Map<string, RunSummary[]>;
  execsByRun: Map<string, ExecutionState[]>;
}

// One expansion level: workflows + selected-child children + selected-
// child executions for a list of frames. Hook — must be called a fixed
// number of times (the 3 level slots below).
function useSubbotLevel(
  frames: FrameSpec[],
  pickedByFrame: Map<string, string>,
): LevelData {
  // Effective selection: the user's pick while that child still exists,
  // else the live/latest child (not children[0] — that is the oldest).
  const selected = useMemo(() => {
    const m = new Map<string, string>();
    for (const f of frames) {
      const id = resolveSelectedSubbotChild(
        f.children,
        pickedByFrame.get(f.frameId),
      );
      if (id) m.set(f.frameId, id);
    }
    return m;
  }, [frames, pickedByFrame]);

  const statusById = useMemo(() => {
    const m = new Map<string, RunSummary["status"]>();
    for (const f of frames) for (const c of f.children) m.set(c.id, c.status);
    return m;
  }, [frames]);

  const combineWf = useCallback(
    (results: UseQueryResult<WireWorkflow, Error>[]) => {
      const m = new Map<string, WireWorkflow>();
      results.forEach((r, i) => {
        if (r.data) m.set(frames[i]!.frameId, r.data);
      });
      return m;
    },
    [frames],
  );
  const wfByFrame = useQueries({
    queries: frames.map((f) => ({
      queryKey: ["run-workflow", f.children[0]!.id],
      queryFn: () => getRunWorkflow(f.children[0]!.id),
      staleTime: 5 * 60_000,
    })),
    combine: combineWf,
  });

  // The SELECTED child's children — the nested level's input + its tab
  // dots. Polls while the selected child (or any of its children) is
  // unsettled.
  const selectedIds = useMemo(
    () => frames.map((f) => selected.get(f.frameId)).filter((id): id is string => !!id),
    [frames, selected],
  );
  const combineChildren = useCallback(
    (results: UseQueryResult<RunSummary[], Error>[]) => {
      const m = new Map<string, RunSummary[]>();
      results.forEach((r, i) => {
        const frame = frames.find((f) => selected.get(f.frameId) === selectedIds[i]);
        if (frame && r.data) m.set(frame.frameId, r.data);
      });
      return m;
    },
    [frames, selected, selectedIds],
  );
  const childrenOfSelected = useQueries({
    queries: selectedIds.map((id) => ({
      queryKey: ["run-children", id],
      queryFn: () => getRunChildren(id),
      refetchInterval: (q: { state: { data?: RunSummary[] } }) => {
        const ownStatus = statusById.get(id);
        const own = ownStatus !== undefined && !isSettledRunStatus(ownStatus);
        const kids = q.state.data?.some((c) => !isSettledRunStatus(c.status));
        return own || kids ? CHILD_POLL_MS : false;
      },
    })),
    combine: combineChildren,
  });

  const combineExecs = useCallback(
    (results: UseQueryResult<RunSnapshot, Error>[]) => {
      const m = new Map<string, ExecutionState[]>();
      results.forEach((r, i) => {
        if (r.data) m.set(selectedIds[i]!, r.data.executions);
      });
      return m;
    },
    [selectedIds],
  );
  const execsByRun = useQueries({
    queries: selectedIds.map((id) => ({
      queryKey: ["subrun-exec", id],
      queryFn: () => getRun(id),
      refetchInterval: (q: { state: { data?: RunSnapshot } }) => {
        const status = q.state.data?.run.status;
        // Poll until the child settles; a settled child's canvas is
        // frozen anyway. Before the first snapshot lands, poll.
        if (status !== undefined && isSettledRunStatus(status)) return false;
        return CHILD_POLL_MS;
      },
    })),
    combine: combineExecs,
  });

  return useMemo(
    () => ({ wfByFrame, selected, childrenOfSelected, execsByRun }),
    [wfByFrame, selected, childrenOfSelected, execsByRun],
  );
}

// deriveNextFrames computes the NESTED level's frames: for each frame,
// the selected child's children grouped by the subbot node of the
// child's workflow that spawned them, keyed by chained expanded id.
// Exported for unit tests.
export function deriveNextFrames(
  frames: FrameSpec[],
  level: Pick<LevelData, "wfByFrame" | "childrenOfSelected">,
): FrameSpec[] {
  const out: FrameSpec[] = [];
  for (const f of frames) {
    const wf = level.wfByFrame.get(f.frameId);
    if (!wf) continue;
    const grand = level.childrenOfSelected.get(f.frameId) ?? [];
    if (grand.length === 0) continue;
    for (const [nodeId, list] of groupChildrenByNode(grand, wf)) {
      if (nodeId === "") continue; // unattributed — never inlined
      out.push({ frameId: makeSubbotChildId(f.frameId, nodeId), children: list });
    }
  }
  return out.slice(0, MAX_FRAMES_PER_LEVEL);
}

export function useInlineSubbotData(
  rootChildrenByNode: Map<string, RunSummary[]>,
  pickedByFrame: Map<string, string>,
): InlineSubbotData {
  const level1Frames = useMemo<FrameSpec[]>(
    () =>
      Array.from(rootChildrenByNode)
        .filter(([nodeId, list]) => nodeId !== "" && list.length > 0)
        .map(([nodeId, list]) => ({ frameId: nodeId, children: list }))
        .sort((a, b) => (a.frameId < b.frameId ? -1 : 1))
        .slice(0, MAX_FRAMES_PER_LEVEL),
    [rootChildrenByNode],
  );

  const l1 = useSubbotLevel(level1Frames, pickedByFrame);
  const level2Frames = useMemo(
    () => deriveNextFrames(level1Frames, l1),
    [level1Frames, l1],
  );
  const l2 = useSubbotLevel(level2Frames, pickedByFrame);
  const level3Frames = useMemo(
    () => deriveNextFrames(level2Frames, l2),
    [level2Frames, l2],
  );
  const l3 = useSubbotLevel(level3Frames, pickedByFrame);

  return useMemo(() => {
    if (level1Frames.length === 0) return EMPTY;
    const subRunsByNode = new Map<string, RunSummary[]>();
    for (const f of [...level1Frames, ...level2Frames, ...level3Frames]) {
      subRunsByNode.set(f.frameId, f.children);
    }
    const childWorkflowsByNode = new Map([
      ...l1.wfByFrame,
      ...l2.wfByFrame,
      ...l3.wfByFrame,
    ]);
    const childExecutionsByRun = new Map([
      ...l1.execsByRun,
      ...l2.execsByRun,
      ...l3.execsByRun,
    ]);
    const selectedChildByFrame = new Map([
      ...l1.selected,
      ...l2.selected,
      ...l3.selected,
    ]);
    return {
      subRunsByNode,
      childWorkflowsByNode,
      childExecutionsByRun,
      selectedChildByFrame,
    };
  }, [level1Frames, level2Frames, level3Frames, l1, l2, l3]);
}
