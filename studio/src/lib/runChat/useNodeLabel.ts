import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type WireWorkflow } from "@/api/runs";
import { useRunStore } from "@/store/run";
import { humanizeNodeId } from "./nodeKindResolver";

// useNodeLabel returns a labeller for the ACTIVE run's nodes: the
// authored `description:` when the workflow declares one, else the
// humanizeNodeId heuristic. It shares the ["run-workflow", runId] query
// (staleTime: Infinity) with useRunChatMessages, so components deep in
// the run console (StatusHero, DetailHeader) get authored labels
// without threading the workflow through props — before the workflow
// loads, the heuristic answers.
export function useNodeLabel(): (nodeId: string) => string {
  const runId = useRunStore((s) => s.runId);
  const workflowQuery = useQuery<WireWorkflow>({
    queryKey: ["run-workflow", runId],
    queryFn: () => getRunWorkflow(runId!),
    enabled: !!runId,
    staleTime: Infinity,
  });
  const wf = workflowQuery.data ?? null;
  return useMemo(() => {
    const byId = new Map<string, string>();
    for (const n of wf?.nodes ?? []) {
      const desc = n.description?.trim();
      if (desc) byId.set(n.id, desc);
    }
    return (nodeId: string) => byId.get(nodeId) ?? humanizeNodeId(nodeId);
  }, [wf]);
}
