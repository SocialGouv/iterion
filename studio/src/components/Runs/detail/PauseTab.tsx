import { useMemo } from "react";

import type { RunEvent } from "@/api/runs";
import { isAsyncHumanInput } from "@/api/runs";

import PauseForm from "../PauseForm";

interface PauseInfo {
  questions: Record<string, unknown>;
  message?: string;
}

function usePauseInfo(matching: RunEvent[]): PauseInfo | null {
  return useMemo<PauseInfo | null>(() => {
    // Walk newest → oldest looking for the most recent
    // human_input_requested for this execution. The reducer in the
    // store flips status back to running on resume, so it's safe to
    // assume the latest pause request is the active one.
    for (let i = matching.length - 1; i >= 0; i--) {
      const e = matching[i]!;
      // Async questions (ADR-081) never pause the run — they must not
      // masquerade as the active pause form.
      if (isAsyncHumanInput(e)) continue;
      if (e.type === "human_input_requested" && e.data) {
        return {
          questions:
            (e.data["questions"] as Record<string, unknown> | undefined) ?? {},
          message:
            (e.data["message"] as string | undefined) ??
            (e.data["reason"] as string | undefined),
        };
      }
    }
    return null;
  }, [matching]);
}

export function PauseTab({
  runId,
  matching,
}: {
  runId: string;
  matching: RunEvent[];
}) {
  const pause = usePauseInfo(matching);
  return (
    <div className="overflow-auto px-4 py-3 h-full">
      <PauseForm
        runId={runId}
        questions={pause?.questions ?? {}}
        message={pause?.message}
      />
    </div>
  );
}
