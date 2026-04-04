import { useEffect, useCallback, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useSelectionStore } from "@/store/selection";
import { findNodeDecl } from "@/lib/defaults";
import { NODE_ICONS, NODE_COLORS } from "@/lib/constants";
import type { AgentDecl, JudgeDecl, RouterDecl, HumanDecl, ToolNodeDecl, NodeKind } from "@/api/types";
import AgentForm from "@/components/Panels/forms/AgentForm";
import RouterForm from "@/components/Panels/forms/RouterForm";
import HumanForm from "@/components/Panels/forms/HumanForm";
import ToolForm from "@/components/Panels/forms/ToolForm";
import ConfirmDialog from "@/components/shared/ConfirmDialog";

export default function EditNodeModal() {
  const document = useDocumentStore((s) => s.document);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const detailNodeId = useUIStore((s) => s.detailNodeId);
  const setDetailNodeId = useUIStore((s) => s.setDetailNodeId);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const [confirmDelete, setConfirmDelete] = useState(false);

  // Keep selection in sync so forms that use useSelectionStore work correctly
  useEffect(() => {
    if (detailNodeId) setSelectedNode(detailNodeId);
  }, [detailNodeId, setSelectedNode]);

  const close = useCallback(() => {
    setDetailNodeId(null);
  }, [setDetailNodeId]);

  // ESC to close
  useEffect(() => {
    if (!detailNodeId) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        close();
      }
    };
    window.addEventListener("keydown", handler, true);
    return () => window.removeEventListener("keydown", handler, true);
  }, [detailNodeId, close]);

  if (!detailNodeId || !document) return null;

  // Terminal / start nodes — read-only
  const isTerminal = detailNodeId === "done" || detailNodeId === "fail" || detailNodeId === "__start__";
  if (isTerminal) {
    const icon = detailNodeId === "__start__" ? "\u{25B6}\u{FE0F}" : detailNodeId === "done" ? "\u{2705}" : "\u{274C}";
    const label = detailNodeId === "__start__" ? "Start" : detailNodeId;
    return (
      <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50" onClick={close}>
        <div className="bg-gray-800 border border-gray-600 rounded-lg p-4 w-[360px]" onClick={(e) => e.stopPropagation()}>
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2">
              <span className="text-lg">{icon}</span>
              <span className="text-sm font-bold text-white">{label}</span>
            </div>
            <button className="text-gray-400 hover:text-white text-lg px-1" onClick={close}>&times;</button>
          </div>
          <p className="text-xs text-gray-500">Terminal node — no editable properties.</p>
        </div>
      </div>
    );
  }

  const found = findNodeDecl(document, detailNodeId);
  if (!found) return null;
  const { kind, decl } = found;

  const handleDelete = () => {
    removeNode(detailNodeId);
    clearSelection();
    setConfirmDelete(false);
    close();
  };

  const color = NODE_COLORS[kind] ?? "#6B7280";
  const icon = NODE_ICONS[kind] ?? "";

  return (
    <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50" onClick={close}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg w-[440px] max-h-[80vh] flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-4 py-3 border-b border-gray-700 shrink-0">
          <div className="flex items-center gap-2">
            <span className="text-lg">{icon}</span>
            <div>
              <div className="font-semibold text-white text-sm">{detailNodeId}</div>
              <div className="text-[10px] uppercase tracking-wider font-medium" style={{ color }}>{kind}</div>
            </div>
          </div>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={close} title="Close (Esc)">&times;</button>
        </div>

        {/* Form body */}
        <div className="flex-1 overflow-y-auto px-4 py-3">
          <NodeForm kind={kind} decl={decl} />
        </div>

        {/* Footer */}
        <div className="px-4 py-2 border-t border-gray-700 shrink-0">
          <button
            className="w-full bg-red-900/60 hover:bg-red-800 text-red-200 text-xs py-1.5 rounded"
            onClick={() => setConfirmDelete(true)}
          >
            Delete Node
          </button>
        </div>

        <ConfirmDialog
          open={confirmDelete}
          title="Delete Node"
          message={`Delete "${detailNodeId}"? This will also remove all edges connected to it.`}
          confirmLabel="Delete"
          confirmVariant="danger"
          onConfirm={handleDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      </div>
    </div>
  );
}

function NodeForm({ kind, decl }: { kind: NodeKind; decl: AgentDecl | JudgeDecl | RouterDecl | HumanDecl | ToolNodeDecl }) {
  switch (kind) {
    case "agent":
      return <AgentForm decl={decl as AgentDecl} kind="agent" />;
    case "judge":
      return <AgentForm decl={decl as JudgeDecl} kind="judge" />;
    case "router":
      return <RouterForm decl={decl as RouterDecl} />;
    case "human":
      return <HumanForm decl={decl as HumanDecl} />;
    case "tool":
      return <ToolForm decl={decl as ToolNodeDecl} />;
    default:
      return <p className="text-gray-500 text-xs">No editable properties</p>;
  }
}
