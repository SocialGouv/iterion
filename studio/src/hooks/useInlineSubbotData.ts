import { useCallback, useMemo } from "react";
import { useQueries, type UseQueryResult } from "@tanstack/react-query";

import {
  getRun,
  getRunWorkflow,
  type ExecutionState,
  type RunSnapshot,
  type RunSummary,
  type WireWorkflow,
} from "@/api/runs";
import { isSettledRunStatus } from "@/lib/subRuns";

// Data feeds for the run canvas's INLINE subbot expansion
// (lib/subbotRunGraph): per subbot node, the child workflow shape (all
// children of one node share the source — fetched once from the first
// child), and per child run, its live execution states (REST-polled at
// 3s while unsettled; the WS-live focused view is the sub-run tab).
const CHILD_EXEC_POLL_MS = 3000;
// Safety cap on concurrently polled children — beyond this the inline
// frame still renders, later children just show no live pips (their
// tab remains fully live).
const MAX_POLLED_CHILDREN = 16;

export interface InlineSubbotData {
  childWorkflowsByNode: Map<string, WireWorkflow>;
  childExecutionsByRun: Map<string, ExecutionState[]>;
}

const EMPTY: InlineSubbotData = {
  childWorkflowsByNode: new Map(),
  childExecutionsByRun: new Map(),
};

export function useInlineSubbotData(
  childrenByNode: Map<string, RunSummary[]>,
): InlineSubbotData {
  // Attributed subbot nodes only (the "" bucket holds shard/legacy
  // children that never render inline).
  const pairs = useMemo(
    () =>
      Array.from(childrenByNode)
        .filter(([nodeId, list]) => nodeId !== "" && list.length > 0)
        .map(([nodeId, list]) => ({ nodeId, firstChildId: list[0]!.id }))
        .sort((a, b) => (a.nodeId < b.nodeId ? -1 : 1)),
    [childrenByNode],
  );

  const combineWf = useCallback(
    (results: UseQueryResult<WireWorkflow, Error>[]) => {
      const m = new Map<string, WireWorkflow>();
      results.forEach((r, i) => {
        if (r.data) m.set(pairs[i]!.nodeId, r.data);
      });
      return m;
    },
    [pairs],
  );
  const childWorkflowsByNode = useQueries({
    queries: pairs.map((p) => ({
      queryKey: ["run-workflow", p.firstChildId],
      queryFn: () => getRunWorkflow(p.firstChildId),
      staleTime: 5 * 60_000,
    })),
    combine: combineWf,
  });

  const polledChildren = useMemo(
    () =>
      Array.from(childrenByNode)
        .filter(([nodeId]) => nodeId !== "")
        .flatMap(([, list]) => list)
        .slice(0, MAX_POLLED_CHILDREN),
    [childrenByNode],
  );

  const combineExecs = useCallback(
    (results: UseQueryResult<RunSnapshot, Error>[]) => {
      const m = new Map<string, ExecutionState[]>();
      results.forEach((r, i) => {
        if (r.data) m.set(polledChildren[i]!.id, r.data.executions);
      });
      return m;
    },
    [polledChildren],
  );
  const childExecutionsByRun = useQueries({
    queries: polledChildren.map((c) => ({
      queryKey: ["subrun-exec", c.id],
      queryFn: () => getRun(c.id),
      refetchInterval: (q: { state: { data?: RunSnapshot } }) => {
        const status = q.state.data?.run.status;
        // Poll until the child settles; a settled child's canvas is
        // frozen anyway. Before the first snapshot lands, poll.
        if (status !== undefined && isSettledRunStatus(status)) return false;
        return CHILD_EXEC_POLL_MS;
      },
    })),
    combine: combineExecs,
  });

  return useMemo(() => {
    if (pairs.length === 0) return EMPTY;
    return { childWorkflowsByNode, childExecutionsByRun };
  }, [pairs, childWorkflowsByNode, childExecutionsByRun]);
}
