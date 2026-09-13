import { useMemo, useState } from "react";
import type { IterDocument, PortGraphDecl, WorkflowDecl } from "@/api/types";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { Button } from "@/components/ui";
import { CommittedTextField, SelectField, TextField } from "./forms/FormField";
import PublicContractPanel from "@/components/Inspector/PublicContractPanel";
import PublicContractEditor from "@/components/Inspector/PublicContractEditor";
import { effectiveNativePortTypes } from "@/lib/nativePorts";

type Endpoint = { name: string; type: string };

function outputEndpoints(doc: IterDocument, wf: WorkflowDecl): Endpoint[] {
  const contracts = new Map((doc.contracts ?? []).map(contract => [contract.name, contract]));
  const effective = effectiveNativePortTypes(wf, doc.contracts ?? []);
  const root = contracts.get(wf.contract ?? "");
  return [
    ...(root?.inputs ?? []).map(port => ({ name: `input.${port.name}`, type: effective.get(`input.${port.name}`) ?? port.type })),
    ...(wf.graph?.nodes ?? []).flatMap(node => (contracts.get(node.contract)?.outputs ?? [])
      .map(port => ({ name: `${node.name}.${port.name}`, type: effective.get(`${node.name}.${port.name}`) ?? port.type }))),
  ];
}

function inputEndpoints(doc: IterDocument, wf: WorkflowDecl): Endpoint[] {
  const contracts = new Map((doc.contracts ?? []).map(contract => [contract.name, contract]));
  return (wf.graph?.nodes ?? []).flatMap(node => (contracts.get(node.contract)?.inputs ?? [])
    .map(port => ({ name: `${node.name}.${port.name}`, type: port.type })));
}

const compatible = (source: string, target: string) => source === target ||
  (source.endsWith("[]") && source.slice(0, -2) === target);

/** Native authoring keeps the graph, public contract and technical policy distinct. */
export default function NativeWorkflowSettings({ workflow }: { workflow: WorkflowDecl }) {
  const document = useDocumentStore((state) => state.document);
  const updateWorkflow = useDocumentStore((state) => state.updateWorkflow);
  const setActiveWorkflowName = useUIStore((state) => state.setActiveWorkflowName);
  const [instanceName, setInstanceName] = useState("");
  const [implementation, setImplementation] = useState("");
  const [nodeContract, setNodeContract] = useState("");
  const [target, setTarget] = useState("");
  const [source, setSource] = useState("");
  const [exportName, setExportName] = useState("");
  const [exportSource, setExportSource] = useState("");
  const graph = workflow.graph ?? {};
  const publicContract = document?.contracts?.find(contract => contract.name === workflow.contract);
  const sources = useMemo(() => document ? outputEndpoints(document, workflow) : [], [document, workflow]);
  const targets = useMemo(() => document ? inputEndpoints(document, workflow) : [], [document, workflow]);
  const targetEndpoint = targets.find(endpoint => endpoint.name === target) ?? targets[0];
  const availableSources = targetEndpoint ? sources.filter(endpoint => compatible(endpoint.type, targetEndpoint.type)) : [];
  const selectedSource = availableSources.find(endpoint => endpoint.name === source) ?? availableSources[0];
  const rootOutputs = publicContract?.outputs ?? [];
  const selectedOutput = rootOutputs.find(port => port.name === exportName) ?? rootOutputs[0];
  const availableExportSources = selectedOutput ? sources.filter(endpoint => endpoint.type === selectedOutput.type) : [];
  const selectedExportSource = availableExportSources.find(endpoint => endpoint.name === exportSource) ?? availableExportSources[0];
  const implementations = [
    ...(document?.agents ?? []), ...(document?.judges ?? []), ...(document?.tools ?? []),
    ...(document?.computes ?? []), ...(document?.subbots ?? []),
  ].map(item => item.name);
  const chosenImplementation = implementations.includes(implementation) ? implementation : implementations[0];
  const contracts = document?.contracts ?? [];
  const chosenContract = contracts.some(item => item.name === nodeContract) ? nodeContract : contracts[0]?.name;
  const updateGraph = (changes: Partial<PortGraphDecl>) => updateWorkflow(workflow.name, { graph: { ...graph, ...changes } });
  const addNode = () => {
    const name = instanceName.trim();
    if (!name || !chosenImplementation || !chosenContract || (graph.nodes ?? []).some(node => node.name === name)) return;
    updateGraph({ nodes: [...(graph.nodes ?? []), { name, implementation: chosenImplementation, contract: chosenContract }] });
    setInstanceName("");
  };
  const addBinding = () => {
    if (!targetEndpoint || !selectedSource || (graph.bindings ?? []).some(binding => binding.to === targetEndpoint.name)) return;
    updateGraph({ bindings: [...(graph.bindings ?? []), { from: selectedSource.name, to: targetEndpoint.name }] });
  };
  const addExport = () => {
    if (!selectedOutput || !selectedExportSource || (graph.exports ?? []).some(item => item.name === selectedOutput.name)) return;
    updateGraph({ exports: [...(graph.exports ?? []), { name: selectedOutput.name, from: selectedExportSource.name }] });
  };
  return (
    <div className="h-full overflow-y-auto p-3 text-sm">
      <h2 className="font-semibold text-fg-default mb-3">Native workflow</h2>
      <CommittedTextField label="Workflow name" value={workflow.name} onChange={(name) => {
        updateWorkflow(workflow.name, { name }); setActiveWorkflowName(name);
      }} />
      <SelectField label="Public contract" value={workflow.contract ?? ""}
        onChange={(contract) => updateWorkflow(workflow.name, { contract })}
        options={contracts.map(item => ({ value: item.name, label: item.display_name || item.name }))} />
      <section className="mt-3 border-t border-border-default pt-3"><PublicContractPanel contract={publicContract} /></section>
      <PublicContractEditor contract={publicContract} />

      <section className="mt-4 border-t border-border-default pt-3">
        <h3 className="font-semibold text-fg-muted">Graph instances</h3>
        <ul className="text-xs text-fg-subtle mt-1 space-y-1">{(graph.nodes ?? []).map(node => (
          <li key={node.name}>{node.name}: {node.contract} → {node.implementation}</li>
        ))}</ul>
        <TextField label="New instance name" value={instanceName} onChange={setInstanceName} />
        <SelectField label="Implementation" value={chosenImplementation ?? ""} onChange={setImplementation}
          options={implementations.map(name => ({ value: name, label: name }))} />
        <SelectField label="Node contract" value={chosenContract ?? ""} onChange={setNodeContract}
          options={contracts.map(item => ({ value: item.name, label: item.display_name || item.name }))} />
        <Button size="sm" onClick={addNode} disabled={!instanceName.trim() || !chosenImplementation || !chosenContract}>Add instance</Button>
      </section>

      <section className="mt-4 border-t border-border-default pt-3">
        <h3 className="font-semibold text-fg-muted">Named port bindings</h3>
        <ul className="text-xs text-fg-subtle mt-1 space-y-1">{(graph.bindings ?? []).map(binding => (
          <li key={binding.to}>{binding.from} → {binding.to}</li>
        ))}</ul>
        <SelectField label="Input to supply" value={targetEndpoint?.name ?? ""} onChange={setTarget}
          options={targets.map(endpoint => ({ value: endpoint.name, label: `${endpoint.name}: ${endpoint.type}` }))} />
        <SelectField label="Source output" value={selectedSource?.name ?? ""} onChange={setSource}
          options={availableSources.map(endpoint => ({ value: endpoint.name, label: `${endpoint.name}: ${endpoint.type}` }))} />
        <Button size="sm" onClick={addBinding} disabled={!targetEndpoint || !selectedSource ||
          (graph.bindings ?? []).some(binding => binding.to === targetEndpoint.name)}>Bind ports</Button>
        {selectedSource?.type.endsWith("[]") && selectedSource.type.slice(0, -2) === targetEndpoint?.type &&
          <p className="text-caption text-fg-subtle mt-1">Each array item launches one invocation; the item count is known at runtime.</p>}
      </section>

      <section className="mt-4 border-t border-border-default pt-3">
        <h3 className="font-semibold text-fg-muted">Workflow outputs</h3>
        <ul className="text-xs text-fg-subtle mt-1 space-y-1">{(graph.exports ?? []).map(item => (
          <li key={item.name}>{item.name} ← {item.from}{(graph.products ?? []).includes(item.name) ? " · product" : ""}</li>
        ))}</ul>
        <SelectField label="Public output" value={selectedOutput?.name ?? ""} onChange={setExportName}
          options={rootOutputs.map(port => ({ value: port.name, label: `${port.name}: ${port.type}` }))} />
        <SelectField label="Produced by" value={selectedExportSource?.name ?? ""} onChange={setExportSource}
          options={availableExportSources.map(endpoint => ({ value: endpoint.name, label: `${endpoint.name}: ${endpoint.type}` }))} />
        <Button size="sm" onClick={addExport} disabled={!selectedOutput || !selectedExportSource ||
          (graph.exports ?? []).some(item => item.name === selectedOutput.name)}>Export output</Button>
        <p className="text-caption text-fg-subtle mt-1">Mark an exported output as a product by selecting its connection.</p>
      </section>

      <details className="mt-4 border-t border-border-default pt-3">
        <summary className="cursor-pointer text-xs font-semibold text-fg-muted uppercase tracking-wide">Technical workflow configuration</summary>
        <p className="text-caption text-fg-subtle mt-2">Policy: {workflow.port_policy || "none"}</p>
        <pre className="mt-2 overflow-x-auto text-caption text-fg-subtle">{JSON.stringify({
          budget: workflow.budget, resources: workflow.resources, default_backend: workflow.default_backend,
          tool_policy: workflow.tool_policy, worktree: workflow.worktree, sandbox: workflow.sandbox,
          port_policy: document?.port_policies?.find(policy => policy.name === workflow.port_policy),
        }, null, 2)}</pre>
      </details>
    </div>
  );
}
