import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { makeEdgeId } from "@/lib/documentToGraph";
import type { Edge } from "@/api/types";
import EdgeForm from "@/components/Panels/forms/EdgeForm";
import { IconButton } from "@/components/ui";
import { TrashIcon } from "@radix-ui/react-icons";

interface EdgeMatch {
  edge: Edge;
  edgeIndex: number;
  workflowName: string;
}

export default function InspectorEdge({ edgeId }: { edgeId: string }) {
  const document = useDocumentStore((s) => s.document);
  const removeEdge = useDocumentStore((s) => s.removeEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);

  const match = useMemo<EdgeMatch | null>(() => {
    if (!document) return null;
    for (const wf of document.workflows ?? []) {
      const wfEdges = wf.edges ?? [];
      for (let i = 0; i < wfEdges.length; i++) {
        const e = wfEdges[i];
        if (!e) continue;
        if (makeEdgeId(wf.name, i) === edgeId) {
          return { edge: e, edgeIndex: i, workflowName: wf.name };
        }
      }
    }
    return null;
  }, [document, edgeId]);

  if (!match) {
    return (
      <div className="p-3 text-xs text-fg-subtle">Edge not found.</div>
    );
  }

  const { edge, edgeIndex, workflowName } = match;

  const handleDelete = () => {
    removeEdge(workflowName, edgeIndex);
    clearSelection();
  };

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center gap-2 border-b border-border-default px-3 py-2 shrink-0">
        <span className="text-base shrink-0">{"\u{1F517}"}</span>
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold text-fg-default truncate">
            {edge.from} <span className="text-fg-subtle">{"\u2192"}</span> {edge.to}
          </div>
          <div className="text-[10px] uppercase tracking-wider text-fg-subtle">Edge</div>
        </div>
        <IconButton
          variant="ghost"
          size="sm"
          label="Delete edge"
          onClick={handleDelete}
        >
          <TrashIcon />
        </IconButton>
      </div>
      <div className="flex-1 overflow-y-auto p-3">
        <EdgeForm edge={edge} edgeIndex={edgeIndex} workflowName={workflowName} />
      </div>
    </div>
  );
}
