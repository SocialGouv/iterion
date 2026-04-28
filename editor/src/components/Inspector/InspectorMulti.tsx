import { useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isGroupNodeId } from "@/lib/groups";
import { makeEdgeId } from "@/lib/documentToGraph";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { Button } from "@/components/ui";
import { TrashIcon } from "@radix-ui/react-icons";

interface InspectorMultiProps {
  nodeIds: string[];
  edgeIds: string[];
}

function isEditableNodeId(id: string): boolean {
  return (
    id !== "__start__" &&
    id !== "done" &&
    id !== "fail" &&
    !isAuxiliaryNodeId(id) &&
    !isGroupNodeId(id)
  );
}

export default function InspectorMulti({ nodeIds, edgeIds }: InspectorMultiProps) {
  const document = useDocumentStore((s) => s.document);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const removeEdge = useDocumentStore((s) => s.removeEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const editableNodes = nodeIds.filter(isEditableNodeId);
  const totalDeletable = editableNodes.length + edgeIds.length;

  const handleDelete = () => {
    // Resolve edge ids to (workflowName, edgeIndex) before any mutation, since
    // indices shift as we remove.
    const edgeRefs: { workflowName: string; edgeIndex: number }[] = [];
    if (document) {
      for (const wf of document.workflows ?? []) {
        const wfEdges = wf.edges ?? [];
        for (let i = 0; i < wfEdges.length; i++) {
          if (edgeIds.includes(makeEdgeId(wf.name, i))) {
            edgeRefs.push({ workflowName: wf.name, edgeIndex: i });
          }
        }
      }
    }
    // Remove edges from highest index downward to keep remaining indices valid.
    edgeRefs.sort((a, b) =>
      a.workflowName === b.workflowName
        ? b.edgeIndex - a.edgeIndex
        : a.workflowName.localeCompare(b.workflowName),
    );
    for (const r of edgeRefs) removeEdge(r.workflowName, r.edgeIndex);
    for (const id of editableNodes) removeNode(id);
    clearSelection();
    setConfirmDelete(false);
  };

  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center gap-2 border-b border-border-default px-3 py-2 shrink-0">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold text-fg-default">
            {nodeIds.length + edgeIds.length} selected
          </div>
          <div className="text-[10px] uppercase tracking-wider text-fg-subtle">
            {nodeIds.length} node{nodeIds.length === 1 ? "" : "s"} ·{" "}
            {edgeIds.length} edge{edgeIds.length === 1 ? "" : "s"}
          </div>
        </div>
      </div>
      <div className="flex-1 overflow-y-auto p-3 space-y-3">
        <section>
          <h3 className="text-xs uppercase tracking-wider text-fg-subtle mb-1">Nodes</h3>
          {nodeIds.length === 0 ? (
            <p className="text-xs text-fg-subtle">No nodes selected.</p>
          ) : (
            <ul className="text-xs text-fg-default space-y-0.5">
              {nodeIds.map((id) => (
                <li
                  key={id}
                  className={`truncate ${isEditableNodeId(id) ? "" : "text-fg-subtle"}`}
                  title={isEditableNodeId(id) ? id : `${id} (terminal/auxiliary, won't be deleted)`}
                >
                  {id}
                </li>
              ))}
            </ul>
          )}
        </section>
        <section>
          <h3 className="text-xs uppercase tracking-wider text-fg-subtle mb-1">Edges</h3>
          {edgeIds.length === 0 ? (
            <p className="text-xs text-fg-subtle">No edges selected.</p>
          ) : (
            <ul className="text-xs text-fg-default space-y-0.5">
              {edgeIds.map((id) => (
                <li key={id} className="truncate" title={id}>
                  {id}
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
      <div className="border-t border-border-default p-3 shrink-0">
        <Button
          variant="danger"
          size="sm"
          leadingIcon={<TrashIcon />}
          onClick={() => setConfirmDelete(true)}
          disabled={totalDeletable === 0}
          className="w-full"
        >
          Delete {totalDeletable} item{totalDeletable === 1 ? "" : "s"}
        </Button>
      </div>
      <ConfirmDialog
        open={confirmDelete}
        title="Delete Selection"
        message={`Delete ${editableNodes.length} node${editableNodes.length === 1 ? "" : "s"} and ${edgeIds.length} edge${edgeIds.length === 1 ? "" : "s"}? Edges connected to deleted nodes will also be removed.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={handleDelete}
        onCancel={() => setConfirmDelete(false)}
      />
    </div>
  );
}
