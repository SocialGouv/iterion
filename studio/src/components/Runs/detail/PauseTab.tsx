import { useMemo } from "react";

import type { RunEvent } from "@/api/runs";

import HumanPromptForm from "../conversation/HumanPromptForm";
import MarkdownText from "../conversation/MarkdownText";

interface PauseInfo {
  questions: Record<string, unknown>;
  instructions?: string;
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
      if (e.type === "human_input_requested" && e.data) {
        return {
          questions:
            (e.data["questions"] as Record<string, unknown> | undefined) ?? {},
          instructions: e.data["instructions"] as string | undefined,
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
  nodeId,
  matching,
}: {
  runId: string;
  nodeId: string;
  matching: RunEvent[];
}) {
  const pause = usePauseInfo(matching);
  const context = pause?.instructions ?? pause?.message;
  return (
    <div className="overflow-auto px-4 py-3 h-full space-y-3">
      {context && (
        <div className="rounded-md border border-border-subtle bg-surface-1 px-3 py-2 text-fg-default">
          <MarkdownText value={context} size="sm" />
        </div>
      )}
      <HumanPromptForm
        runId={runId}
        nodeId={nodeId}
        questions={pause?.questions ?? {}}
      />
    </div>
  );
}
