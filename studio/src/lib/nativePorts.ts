import type { ContractDecl, PortGraphDecl, WorkflowDecl } from "@/api/types";

/** A readable cardinality preview for an incomplete editor graph. Go's
 * compiler remains the authority for schema identity and graph validity. */
export function effectiveNativePortTypes(workflow: WorkflowDecl, contracts: ContractDecl[]): Map<string, string> {
  const graph: PortGraphDecl = workflow.graph ?? {};
  const byContract = new Map(contracts.map(contract => [contract.name, contract]));
  const types = new Map<string, string>();
  for (const port of byContract.get(workflow.contract ?? "")?.inputs ?? []) {
    types.set(`input.${port.name}`, port.type);
  }
  const nodes = graph.nodes ?? [];
  const mapped = new Set<string>();
  const declared = new Map(nodes.map(node => [node.name, byContract.get(node.contract)]));
  const updateOutputs = (node: string) => {
    for (const port of declared.get(node)?.outputs ?? []) {
      types.set(`${node}.${port.name}`, mapped.has(node) ? `${port.type}[]` : port.type);
    }
  };
  for (const node of nodes) updateOutputs(node.name);
  for (let pass = 0; pass < nodes.length; pass++) {
    let changed = false;
    for (const binding of graph.bindings ?? []) {
      const dot = binding.to.indexOf(".");
      if (dot <= 0) continue;
      const node = binding.to.slice(0, dot);
      const input = declared.get(node)?.inputs?.find(port => port.name === binding.to.slice(dot + 1));
      const source = types.get(binding.from);
      if (!input || !source?.endsWith("[]") || source.slice(0, -2) !== input.type || mapped.has(node)) continue;
      mapped.add(node);
      updateOutputs(node);
      changed = true;
    }
    if (!changed) break;
  }
  return types;
}
