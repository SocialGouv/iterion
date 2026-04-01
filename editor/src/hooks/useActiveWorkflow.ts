import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import type { WorkflowDecl } from "@/api/types";

/** Returns the currently active workflow based on the UI store's activeWorkflowName. */
export function useActiveWorkflow(): WorkflowDecl | undefined {
  const document = useDocumentStore((s) => s.document);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);

  return useMemo(() => {
    if (!document) return undefined;
    const wfs = document.workflows ?? [];
    if (wfs.length === 0) return undefined;
    if (activeWorkflowName) {
      const found = wfs.find((w) => w.name === activeWorkflowName);
      if (found) return found;
    }
    return wfs[0];
  }, [document, activeWorkflowName]);
}
