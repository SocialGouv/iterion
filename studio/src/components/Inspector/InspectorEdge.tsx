import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { makeEdgeId } from "@/lib/documentToGraph";
import type { ContractDecl, Edge, PortGraphDecl, PublicPortDecl, WorkflowDecl } from "@/api/types";
import EdgeForm from "@/components/Panels/forms/EdgeForm";
import { CheckboxField, CommittedTextField } from "@/components/Panels/forms/FormField";
import { IconButton } from "@/components/ui";
import { TrashIcon } from "@radix-ui/react-icons";
import { effectiveNativePortTypes } from "@/lib/nativePorts";

interface EdgeMatch {
  edge: Edge;
  edgeIndex: number;
  workflowName: string;
}

function bindingHint(
  binding: { from: string; to: string },
  workflow: WorkflowDecl,
  graph: PortGraphDecl | undefined,
  contracts: ContractDecl[],
): string {
  const endpoint = (value: string) => {
    const dot = value.indexOf(".");
    return dot > 0 ? { node: value.slice(0, dot), port: value.slice(dot + 1) } : null;
  };
  const contractFor = (node: string) => {
    const name = node === "input" ? workflow.contract : graph?.nodes?.find(item => item.name === node)?.contract;
    return contracts.find(item => item.name === name);
  };
  const lookup = (value: string, direction: "input" | "output"): PublicPortDecl | undefined => {
    const part = endpoint(value);
    if (!part) return undefined;
    const contract = contractFor(part.node);
    const ports = direction === "input" || part.node === "input" ? contract?.inputs : contract?.outputs;
    return ports?.find(item => item.name === part.port);
  };
  const source = lookup(binding.from, "output");
  const target = lookup(binding.to, "input");
  if (!source || !target) return "Validate the workflow to resolve this connection's cardinality.";
  const effective = effectiveNativePortTypes(workflow, contracts);
  const sourceType = effective.get(binding.from) ?? source.type;
  const mapped = sourceType.endsWith("[]") && sourceType.slice(0, -2) === target.type;
  if (mapped) {
    const lifted = sourceType !== source.type;
    const range = lifted ? "item count is dynamic" : source.max_items === undefined
      ? source.min_items === undefined ? "item count is dynamic" : `at least ${source.min_items} invocations; maximum unknown`
      : `${source.min_items ?? 0}–${source.max_items} invocations`;
    const targetNode = endpoint(binding.to)?.node;
    const paid = contractFor(targetNode ?? "")?.effects?.some(effect => effect.paid);
    return `One invocation per array element (${range}); ready items may run in parallel within the workflow limit.${paid ? " Paid effect cost is unknown until execution." : ""}`;
  }
  if (sourceType === target.type && sourceType.endsWith("[]")) {
    return "The complete array is passed as one value; this connection does not expand the node.";
  }
  if (sourceType !== target.type) return "The declared port types differ; validate the workflow before execution.";
  const targetNode = endpoint(binding.to)?.node;
  const siblingMap = (graph?.bindings ?? []).some(item => {
    if (item.to === binding.to || endpoint(item.to)?.node !== targetNode) return false;
    const supplied = lookup(item.from, "output");
    const consumed = lookup(item.to, "input");
    const suppliedType = effective.get(item.from) ?? supplied?.type;
    return !!suppliedType && !!consumed && suppliedType.endsWith("[]") && suppliedType.slice(0, -2) === consumed.type;
  });
  return siblingMap ? "This value is broadcast to every mapped invocation of the node." : "The value is delivered after its supplier commits.";
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
        {binding && <p className="text-caption text-fg-subtle mt-3">{bindingHint(binding, activeWorkflow, graph, document?.contracts ?? [])}</p>}
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
