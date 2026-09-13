import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { makeEdgeId } from "@/lib/documentToGraph";
import type { Edge } from "@/api/types";
import EdgeForm from "@/components/Panels/forms/EdgeForm";
import { CheckboxField, CommittedTextField } from "@/components/Panels/forms/FormField";
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
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const activeWorkflow = useActiveWorkflow();

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

  if (activeWorkflow?.runtime_semantics === "ports-v1") {
    const graph = activeWorkflow.graph;
    const bindings = graph?.bindings ?? [];
    const exports = graph?.exports ?? [];
    const bindingIndex = bindings.findIndex((_, index) => edgeId === `${activeWorkflow.name}:port:${index}`);
    const exportIndex = exports.findIndex((_, index) => edgeId === `${activeWorkflow.name}:export:${index}`);
    const binding = bindings[bindingIndex];
    const exported = exports[exportIndex];
    if (!binding && !exported) return <div className="p-3 text-xs text-fg-subtle">Connection not found.</div>;
    const changeBinding = (updates: Partial<{ from: string; to: string }>) => {
      updateWorkflow(activeWorkflow.name, { graph: { ...graph, bindings: bindings.map((item, index) =>
        index === bindingIndex ? { ...item, ...updates } : item) } });
    };
    const changeExport = (updates: Partial<{ name: string; from: string }>) => {
      updateWorkflow(activeWorkflow.name, { graph: { ...graph, exports: exports.map((item, index) =>
        index === exportIndex ? { ...item, ...updates } : item) } });
    };
    const deleteConnection = () => {
      updateWorkflow(activeWorkflow.name, { graph: binding ? { ...graph, bindings: bindings.filter((_, index) => index !== bindingIndex) }
        : { ...graph, exports: exports.filter((_, index) => index !== exportIndex),
          products: (graph?.products ?? []).filter((name) => name !== exported?.name) } });
      clearSelection();
    };
    const product = exported && graph?.products?.includes(exported.name);
    return (
      <div className="h-full overflow-y-auto p-3 text-sm">
        <div className="flex items-center justify-between gap-2 mb-3">
          <h2 className="font-semibold text-fg-default">{binding ? "Named port binding" : "Workflow output"}</h2>
          <IconButton variant="ghost" size="sm" label="Delete connection" onClick={deleteConnection}><TrashIcon /></IconButton>
        </div>
        <CommittedTextField label="Source port" value={(binding ?? exported)!.from}
          onChange={(value) => binding ? changeBinding({ from: value }) : changeExport({ from: value })} />
        {binding ? <CommittedTextField label="Input port" value={binding.to}
          onChange={(value) => changeBinding({ to: value })} /> : (
          <>
              <CommittedTextField label="Workflow output name" value={exported!.name}
              onChange={(value) => {
                if (product) updateWorkflow(activeWorkflow.name, { graph: { ...graph,
                  exports: exports.map((item, index) => index === exportIndex ? { ...item, name: value } : item),
                  products: (graph?.products ?? []).map((name) => name === exported!.name ? value : name) } });
                else changeExport({ name: value });
              }} />
            <CheckboxField label="Product of the final workflow" checked={!!product}
              onChange={(checked) => updateWorkflow(activeWorkflow.name,
                { graph: { ...graph, products: checked ? [...(graph?.products ?? []), exported!.name]
                  : (graph?.products ?? []).filter((name) => name !== exported!.name) } })} />
          </>
        )}
        {binding && <p className="text-caption text-fg-subtle mt-3">An array-to-scalar binding expands into independent parallel invocations. Other scalar inputs are broadcast.</p>}
      </div>
    );
  }

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
          <div className="text-caption uppercase tracking-wider text-fg-subtle">Edge</div>
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
