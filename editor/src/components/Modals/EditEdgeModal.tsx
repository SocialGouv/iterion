import { useEffect, useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useSelectionStore } from "@/store/selection";
import { makeEdgeId } from "@/lib/documentToGraph";
import type { Edge } from "@/api/types";
import EdgeForm from "@/components/Panels/forms/EdgeForm";

export default function EditEdgeModal() {
  const document = useDocumentStore((s) => s.document);
  const editModalEdgeInfo = useUIStore((s) => s.editModalEdgeInfo);
  const setEditModalEdgeInfo = useUIStore((s) => s.setEditModalEdgeInfo);
  const removeEdge = useDocumentStore((s) => s.removeEdge);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);

  const close = useCallback(() => {
    setEditModalEdgeInfo(null);
  }, [setEditModalEdgeInfo]);

  // Keep edge selected so EdgeForm works correctly
  useEffect(() => {
    if (editModalEdgeInfo) {
      const edgeId = makeEdgeId(editModalEdgeInfo.workflowName, editModalEdgeInfo.edgeIndex);
      setSelectedEdge(edgeId);
    }
  }, [editModalEdgeInfo, setSelectedEdge]);

  // ESC to close
  useEffect(() => {
    if (!editModalEdgeInfo) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        close();
      }
    };
    window.addEventListener("keydown", handler, true);
    return () => window.removeEventListener("keydown", handler, true);
  }, [editModalEdgeInfo, close]);

  if (!editModalEdgeInfo || !document) return null;

  const { workflowName, edgeIndex } = editModalEdgeInfo;
  const workflow = document.workflows?.find((w) => w.name === workflowName);
  const edge: Edge | undefined = workflow?.edges?.[edgeIndex];

  if (!edge) return null;

  const handleDelete = () => {
    removeEdge(workflowName, edgeIndex);
    close();
  };

  return (
    <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50" onClick={close}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg w-[480px] max-h-[80vh] flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-4 py-3 border-b border-gray-700 shrink-0">
          <div className="flex items-center gap-2">
            <span className="text-lg">{"\u{1F517}"}</span>
            <div>
              <div className="font-semibold text-white text-sm">{edge.from} {"\u2192"} {edge.to}</div>
              <div className="text-[10px] text-gray-400 uppercase tracking-wider">Edge</div>
            </div>
          </div>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={close} title="Close (Esc)">&times;</button>
        </div>

        {/* Form body */}
        <div className="flex-1 overflow-y-auto px-4 py-3">
          <EdgeForm edge={edge} edgeIndex={edgeIndex} workflowName={workflowName} />
        </div>

        {/* Footer */}
        <div className="px-4 py-2 border-t border-gray-700 shrink-0">
          <button
            className="w-full bg-red-900/60 hover:bg-red-800 text-red-200 text-xs py-1.5 rounded"
            onClick={handleDelete}
          >
            Delete Edge
          </button>
        </div>
      </div>
    </div>
  );
}
