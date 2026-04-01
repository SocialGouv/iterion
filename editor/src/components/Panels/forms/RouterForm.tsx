import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import type { RouterDecl, RouterMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import { CommittedTextField, SelectField } from "./FormField";

interface Props {
  decl: RouterDecl;
}

export default function RouterForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateRouter = useDocumentStore((s) => s.updateRouter);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const activeWorkflow = useActiveWorkflow();

  const outgoingEdges = useMemo(() => {
    if (!activeWorkflow) return [];
    return activeWorkflow.edges.filter((e) => e.from === decl.name);
  }, [activeWorkflow, decl.name]);

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#E67E2222", borderLeft: "3px solid #E67E22" }}
      >
        <span className="text-base">{"\u{1F504}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#E67E22" }}>Router</span>
      </div>
      <CommittedTextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
        onCommit={(v) => setSelectedNode(v)}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const names = getAllNodeNames(document!);
          names.delete(decl.name);
          if (names.has(v)) return "Name already exists";
          return null;
        }}
      />
      <SelectField
        label="Mode"
        value={decl.mode}
        onChange={(v) => updateRouter(decl.name, { mode: v as RouterMode })}
        options={[
          { value: "fan_out_all", label: "fan_out_all" },
          { value: "condition", label: "condition" },
        ]}
        help="fan_out_all = send input to all targets in parallel; condition = route based on 'when' clauses on outgoing edges."
      />
      {decl.mode === "condition" && (
        <div className="mt-2 p-2 bg-gray-800 rounded border border-gray-700">
          <p className="text-[10px] text-gray-400 mb-2">
            In condition mode, routing is controlled by &quot;when&quot; clauses on outgoing edges. Click an edge to add conditions.
          </p>
          {outgoingEdges.length === 0 && (
            <p className="text-[10px] text-yellow-400">No outgoing edges yet. Connect this router to target nodes.</p>
          )}
          {outgoingEdges.map((e, i) => (
            <div key={i} className="text-xs text-gray-300 flex items-center gap-1 py-0.5">
              <span className="text-gray-500">&rarr;</span>
              <span>{e.to}</span>
              {e.when ? (
                <span className="text-amber-400 text-[10px]">
                  (when{e.when.negated ? " not" : ""} {e.when.condition})
                </span>
              ) : (
                <span className="text-gray-500 text-[10px]">(no condition)</span>
              )}
            </div>
          ))}
        </div>
      )}
      {decl.mode === "fan_out_all" && outgoingEdges.length > 0 && (
        <p className="text-[10px] text-gray-500 mt-1">
          Sends input to {outgoingEdges.length} target{outgoingEdges.length !== 1 ? "s" : ""} in parallel.
        </p>
      )}
    </div>
  );
}
