import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type WireWorkflow } from "@/api/runs";
import { errorMessage } from "@/lib/errorHints";

// useWorkflowLoad fetches the IR for the run on mount + on runId change.
// Shares the ["run-workflow", runId] cache with RunView / useNodeLabel so
// the canvas reuses an already-fetched IR (default staleTime still
// refetches in the background on mount). Surfaces both the resolved
// workflow and a string error so the canvas can swap between loading /
// error / ready states without owning the fetch lifecycle itself.
export interface WorkflowLoadResult {
  wf: WireWorkflow | null;
  error: string | null;
}

export function useWorkflowLoad(runId: string): WorkflowLoadResult {
  const query = useQuery<WireWorkflow>({
    queryKey: ["run-workflow", runId],
    queryFn: () => getRunWorkflow(runId),
  });
  return {
    wf: query.data ?? null,
    error: query.error ? errorMessage(query.error) : null,
  };
}
