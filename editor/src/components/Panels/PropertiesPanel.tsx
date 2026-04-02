import { useMemo, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { makeEdgeId } from "@/lib/documentToGraph";
import type { AgentDecl, JudgeDecl, RouterDecl, JoinDecl, HumanDecl, ToolNodeDecl, NodeKind, Edge } from "@/api/types";
import AgentForm from "./forms/AgentForm";
import RouterForm from "./forms/RouterForm";
import JoinForm from "./forms/JoinForm";
import HumanForm from "./forms/HumanForm";
import ToolForm from "./forms/ToolForm";
import EdgeForm from "./forms/EdgeForm";
import ConfirmDialog from "../shared/ConfirmDialog";

interface NodeMatch {
  kind: NodeKind;
  decl: AgentDecl | JudgeDecl | RouterDecl | JoinDecl | HumanDecl | ToolNodeDecl;
}

interface EdgeMatch {
  edge: Edge;
  edgeIndex: number;
  workflowName: string;
}

export default function PropertiesPanel() {
  const document = useDocumentStore((s) => s.document);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const nodeMatch = useMemo<NodeMatch | null>(() => {
    if (!document || !selectedNodeId) return null;
    for (const a of document.agents) if (a.name === selectedNodeId) return { kind: "agent", decl: a };
    for (const j of document.judges) if (j.name === selectedNodeId) return { kind: "judge", decl: j };
    for (const r of document.routers) if (r.name === selectedNodeId) return { kind: "router", decl: r };
    for (const j of document.joins) if (j.name === selectedNodeId) return { kind: "join", decl: j };
    for (const h of document.humans) if (h.name === selectedNodeId) return { kind: "human", decl: h };
    for (const t of document.tools) if (t.name === selectedNodeId) return { kind: "tool", decl: t };
    return null;
  }, [document, selectedNodeId]);

  const edgeMatch = useMemo<EdgeMatch | null>(() => {
    if (!document || !selectedEdgeId) return null;
    for (const wf of document.workflows) {
      const wfEdges = wf.edges ?? [];
      for (let i = 0; i < wfEdges.length; i++) {
        const e = wfEdges[i];
        if (!e) continue;
        const id = makeEdgeId(wf.name, i);
        if (id === selectedEdgeId) return { edge: e, edgeIndex: i, workflowName: wf.name };
      }
    }
    return null;
  }, [document, selectedEdgeId]);

  const handleDelete = () => {
    if (selectedNodeId) {
      removeNode(selectedNodeId);
      clearSelection();
      setConfirmDelete(false);
    }
  };

  return (
    <div className="p-3 text-sm h-full flex flex-col">
      <h2 className="font-bold text-gray-300 mb-2">Properties</h2>
      <div className="flex-1 overflow-y-auto">
        {selectedNodeId === "__start__" ? (
          <div className="flex items-center gap-2 px-2 py-3 rounded bg-gray-800/50 border border-gray-700">
            <span className="text-base">{"\u{25B6}\u{FE0F}"}</span>
            <div>
              <p className="text-sm font-bold text-white">Start</p>
              <p className="text-xs text-gray-500">Marks the workflow entry point.</p>
            </div>
          </div>
        ) : selectedNodeId === "done" || selectedNodeId === "fail" ? (
          <div className="flex items-center gap-2 px-2 py-3 rounded bg-gray-800/50 border border-gray-700">
            <span className="text-base">{selectedNodeId === "done" ? "\u{2705}" : "\u{274C}"}</span>
            <div>
              <p className="text-sm font-bold text-white">{selectedNodeId}</p>
              <p className="text-xs text-gray-500">Terminal node — no editable properties.</p>
            </div>
          </div>
        ) : nodeMatch ? (
          <>
            <NodeForm match={nodeMatch} />
            <div className="mt-4 pt-2 border-t border-gray-700">
              <button
                className="w-full bg-red-900 hover:bg-red-800 text-red-200 text-xs py-1 rounded"
                onClick={() => setConfirmDelete(true)}
              >
                Delete Node
              </button>
            </div>
          </>
        ) : edgeMatch ? (
          <EdgeForm
            edge={edgeMatch.edge}
            edgeIndex={edgeMatch.edgeIndex}
            workflowName={edgeMatch.workflowName}
          />
        ) : (
          <p className="text-gray-500 text-xs">Select a node or edge to view its properties.</p>
        )}
      </div>
      <ConfirmDialog
        open={confirmDelete}
        title="Delete Node"
        message={`Delete "${selectedNodeId}"? This will also remove all edges connected to it.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={handleDelete}
        onCancel={() => setConfirmDelete(false)}
      />
    </div>
  );
}

function NodeForm({ match }: { match: NodeMatch }) {
  switch (match.kind) {
    case "agent":
      return <AgentForm decl={match.decl as AgentDecl} kind="agent" />;
    case "judge":
      return <AgentForm decl={match.decl as JudgeDecl} kind="judge" />;
    case "router":
      return <RouterForm decl={match.decl as RouterDecl} />;
    case "join":
      return <JoinForm decl={match.decl as JoinDecl} />;
    case "human":
      return <HumanForm decl={match.decl as HumanDecl} />;
    case "tool":
      return <ToolForm decl={match.decl as ToolNodeDecl} />;
    default:
      return <p className="text-gray-500 text-xs">Terminal node (no editable properties)</p>;
  }
}
