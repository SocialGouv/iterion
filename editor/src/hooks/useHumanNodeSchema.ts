import { useEffect, useState } from "react";

import { getRunWorkflow, type WireSchemaField } from "@/api/runs";

interface State {
  fields: WireSchemaField[] | null; // null while loading or unavailable
  staleHash: boolean;
  error: string | null;
}

const initial: State = { fields: null, staleHash: false, error: null };

/** Fetches the run's compiled workflow and extracts the output_schema
 *  for the given human node. Returns null fields when the workflow is
 *  unavailable or the node has no schema — the panel falls back to a
 *  free-text PauseForm in that case.
 *
 *  Cache is implicit: the backend memoises wire workflows by file
 *  hash (pkg/runview/workflow_export.go), so repeat calls across
 *  paused→resumed→paused cycles are cheap. */
export function useHumanNodeSchema(
  runId: string | null,
  nodeId: string | undefined,
): State {
  const [state, setState] = useState<State>(initial);

  useEffect(() => {
    if (!runId || !nodeId) {
      setState(initial);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const wf = await getRunWorkflow(runId);
        if (cancelled) return;
        const node = wf.nodes.find((n) => n.id === nodeId);
        const fields = node?.output_schema ?? null;
        setState({
          fields,
          staleHash: !!wf.stale_hash,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState({
          fields: null,
          staleHash: false,
          error: (e as Error).message,
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [runId, nodeId]);

  return state;
}
