import type { ContractDecl, PortGraphDecl, PublicPortDecl, WorkflowDecl } from "@/api/types";

export type NativePortEndpoint = { name: string; type: string; port: PublicPortDecl };

/** This preview filters known contradictions. Only validation by the compiler
 * resolves schema equivalence, technical policies and actual output values. */
export function nativePortCanSupply(source: NativePortEndpoint, target: PublicPortDecl, allowMap = true): boolean {
  const mapped = allowMap && source.type.endsWith("[]") && source.type.slice(0, -2) === target.type;
  if (source.type !== target.type && !mapped) return false;
  if (source.port.nullable && (!target.nullable || mapped)) return false;
  if (source.port.required === false && !("default" in source.port) && (target.required !== false || mapped)) return false;
  // A mapped output's collection size comes from the upstream axis, not
  // from the output declaration of one item. Leave those bounds to Go.
  if (!mapped && source.type === source.port.type && source.type.endsWith("[]")) {
    if (source.port.min_items !== undefined && target.max_items !== undefined && source.port.min_items > target.max_items) return false;
    if (source.port.max_items !== undefined && target.min_items !== undefined && source.port.max_items < target.min_items) return false;
  }
  const sourceMedia = source.port.file?.media_type?.split(";", 1)[0]?.trim().toLowerCase();
  const targetMedia = target.file?.media_type?.split(";", 1)[0]?.trim().toLowerCase();
  return !sourceMedia || !targetMedia || sourceMedia === targetMedia;
}

export function nativePortEndpoints(workflow: WorkflowDecl, contracts: ContractDecl[]): {
  sources: NativePortEndpoint[]; targets: NativePortEndpoint[];
} {
  const byContract = new Map(contracts.map(contract => [contract.name, contract]));
  const effective = effectiveNativePortTypes(workflow, contracts);
  const endpoint = (node: string, port: PublicPortDecl, output: boolean): NativePortEndpoint => ({
    name: `${node}.${port.name}`, port,
    type: output ? effective.get(`${node}.${port.name}`) ?? port.type : port.type,
  });
  const nodes = workflow.graph?.nodes ?? [];
  return {
    sources: [
      ...(byContract.get(workflow.contract ?? "")?.inputs ?? []).map(port => endpoint("input", port, true)),
      ...nodes.flatMap(node => (byContract.get(node.contract)?.outputs ?? []).map(port => endpoint(node.name, port, true))),
    ],
    targets: nodes.flatMap(node => (byContract.get(node.contract)?.inputs ?? []).map(port => endpoint(node.name, port, false))),
  };
}

/** Exclude duplicate suppliers, data cycles and a second inferred map axis
 * before offering a new connection. An existing invalid document stays intact. */
export function nativeBindingCanBeAdded(workflow: WorkflowDecl, contracts: ContractDecl[], from: string, to: string): boolean {
  const { sources, targets } = nativePortEndpoints(workflow, contracts);
  const source = sources.find(port => port.name === from);
  const target = targets.find(port => port.name === to);
  if (!source || !target || !nativePortCanSupply(source, target.port)) return false;
  const bindings = workflow.graph?.bindings ?? [];
  if (bindings.some(binding => binding.to === to)) return false;
  const owner = (endpoint: string) => endpoint.slice(0, endpoint.indexOf("."));
  const sourceNode = owner(from), targetNode = owner(to);
  const reachable = new Set<string>();
  const pending = [targetNode];
  while (pending.length) {
    const node = pending.pop();
    if (node === undefined) break;
    if (node === sourceNode) return false;
    if (reachable.has(node)) continue;
    reachable.add(node);
    for (const binding of bindings) if (owner(binding.from) === node) pending.push(owner(binding.to));
  }
  if (source.type !== target.type) {
    for (const binding of bindings) {
      if (owner(binding.to) !== targetNode || binding.from === from) continue;
      const supplied = sources.find(port => port.name === binding.from);
      const consumed = targets.find(port => port.name === binding.to);
      if (supplied?.type.endsWith("[]") && supplied.type.slice(0, -2) === consumed?.type) return false;
    }
  }
  return true;
}

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
