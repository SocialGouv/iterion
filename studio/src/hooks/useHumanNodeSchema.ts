import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useState } from "react";

import { getRunWorkflow, type WireSchemaField } from "@/api/runs";

interface State {
  fields: WireSchemaField[] | null;
  loading: boolean;
  staleHash: boolean;
  error: string | null;
}

export interface HumanNodeSchema extends State {
  // reload re-fetches the run workflow (used by the "Retry" affordance
  // when the first fetch failed — see HumanPromptForm). Stable identity.
  reload: () => void;
}

const initial: State = { fields: null, loading: false, staleHash: false, error: null };

// useHumanNodeSchema returns the output_schema fields for the paused
// human node. Callers MUST distinguish three outcomes, never conflate
// them:
//   - loading=true                      → don't render the form yet
//   - loading=false && error!=null      → the fetch FAILED; surface it
//     and offer reload() — do NOT silently fall back (a fallback form
//     that can't express the gate's verdict strands the operator and
//     drops their notes; iterion#244)
//   - loading=false && error==null && fields empty → the node genuinely
//     has no output schema → free-text PauseForm fallback is correct
export function useHumanNodeSchema(
  runId: string | null,
  nodeId: string | undefined,
): HumanNodeSchema {
  const [state, setState] = useState<State>(initial);
  // Bumped by reload() to force the fetch effect to run again.
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!runId || !nodeId) {
      setState(initial);
      return;
    }
    let cancelled = false;
    setState({ fields: null, loading: true, staleHash: false, error: null });
    (async () => {
      try {
        const wf = await getRunWorkflow(runId);
        if (cancelled) return;
        const node = wf.nodes.find((n) => n.id === nodeId);
        setState({
          fields: node?.output_schema ?? null,
          loading: false,
          staleHash: !!wf.stale_hash,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState({
          fields: null,
          loading: false,
          staleHash: false,
          error: errorMessage(e),
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [runId, nodeId, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  return { ...state, reload };
}
