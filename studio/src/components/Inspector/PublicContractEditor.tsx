import type { ContractDecl, PublicPortDecl } from "@/api/types";
import { useDocumentStore } from "@/store/document";
import { Button } from "@/components/ui";
import { CheckboxField, CommittedTextField, NumberField, SelectField, TextField } from "@/components/Panels/forms/FormField";

const builtInTypes = ["string", "bool", "int", "float", "json", "file"];

function uniqueName(existing: string[], prefix: string) {
  let candidate = prefix;
  let index = 2;
  while (existing.includes(candidate)) candidate = `${prefix}_${index++}`;
  return candidate;
}

/** Authoring is opt-in within the inspector; the compact public view stays legible. */
export default function PublicContractEditor({ contract }: { contract: ContractDecl | undefined }) {
  const document = useDocumentStore((state) => state.document);
  const applyBatch = useDocumentStore((state) => state.applyBatch);
  if (!contract) return null;
  const save = (next: ContractDecl) => applyBatch(doc => ({ ...doc,
    contracts: (doc.contracts ?? []).map(item => item.name === contract.name ? next : item),
  }));
  const updatePort = (direction: "inputs" | "outputs", name: string, changes: Partial<PublicPortDecl>) => save({
    ...contract, [direction]: (contract[direction] ?? []).map(port => port.name === name ? { ...port, ...changes } : port),
  });
  const renamePort = (direction: "inputs" | "outputs", oldName: string, newName: string) => applyBatch(doc => {
    const replacement = direction === "inputs" ? `input.${newName}` : newName;
    return {
      ...doc,
      contracts: (doc.contracts ?? []).map(item => item.name === contract.name ? {
        ...item,
        [direction]: (item[direction] ?? []).map(port => port.name === oldName ? { ...port, name: newName } : port),
        criteria: (item.criteria ?? []).map(criterion => ({ ...criterion,
          port: criterion.port === `${direction === "inputs" ? "input" : "output"}.${oldName}`
            ? `${direction === "inputs" ? "input" : "output"}.${newName}` : criterion.port,
        })),
      } : item),
      workflows: (doc.workflows ?? []).map(workflow => {
        if (!workflow.graph) return workflow;
        const instanceNames = new Set((workflow.graph.nodes ?? [])
          .filter(node => node.contract === contract.name).map(node => node.name));
        const rewriteEndpoint = (endpoint: string, endpointDirection: "source" | "target") => {
          if (workflow.contract === contract.name && direction === "inputs" && endpoint === `input.${oldName}`)
            return replacement;
          const [instance, port] = endpoint.split(".", 2);
          if (port === oldName && instanceNames.has(instance!) &&
              (direction === "inputs" ? endpointDirection === "target" : endpointDirection === "source"))
            return `${instance}.${newName}`;
          return endpoint;
        };
        return { ...workflow, graph: {
          ...workflow.graph,
          bindings: (workflow.graph.bindings ?? []).map(binding => ({
            from: rewriteEndpoint(binding.from, "source"), to: rewriteEndpoint(binding.to, "target"),
          })),
          exports: (workflow.graph.exports ?? []).map(exported => ({
            name: workflow.contract === contract.name && direction === "outputs" && exported.name === oldName
              ? newName : exported.name,
            from: rewriteEndpoint(exported.from, "source"),
          })),
          products: (workflow.graph.products ?? []).map(name => workflow.contract === contract.name &&
            direction === "outputs" && name === oldName ? newName : name),
        } };
      }),
    };
  });
  const removePort = (direction: "inputs" | "outputs", name: string) => save({
    ...contract, [direction]: (contract[direction] ?? []).filter(port => port.name !== name),
  });
  const addPort = (direction: "inputs" | "outputs") => {
    const ports = contract[direction] ?? [];
    const name = uniqueName(ports.map(port => port.name), direction === "inputs" ? "input" : "output");
    save({ ...contract, [direction]: [...ports, { name, type: "string" }] });
  };
  const typeNames = [
    ...builtInTypes, ...builtInTypes.filter(type => type !== "file").map(type => `${type}[]`),
    ...(document?.schemas ?? []).flatMap(schema => [schema.name, `${schema.name}[]`]),
  ];
  return (
    <details className="mt-4 border-t border-border-default pt-3" data-testid="public-contract-editor">
      <summary className="cursor-pointer text-xs font-semibold text-fg-muted uppercase tracking-wide">Edit public contract</summary>
      <div className="mt-3 text-xs">
        <CommittedTextField label="Public name" value={contract.display_name}
          onChange={value => save({ ...contract, display_name: value })} />
        <TextField label="Responsibility" value={contract.responsibility} multiline
          onChange={value => save({ ...contract, responsibility: value })} />
        {(["inputs", "outputs"] as const).map(direction => (
          <section key={direction} className="mt-4 border-t border-border-default pt-3">
            <h3 className="font-semibold text-fg-muted uppercase tracking-wide">{direction}</h3>
            {(contract[direction] ?? []).map(port => (
              <div key={port.name} className="rounded border border-border-default p-2 mt-2">
                <CommittedTextField label="Port name" value={port.name}
                  onChange={value => renamePort(direction, port.name, value)}
                  validate={value => !/^[A-Za-z_][A-Za-z0-9_]*$/.test(value) ? "Use an identifier without dots" :
                    (contract[direction] ?? []).some(other => other.name === value && other.name !== port.name)
                      ? "Port name already exists" : null} />
                <SelectField label="Type" value={port.type} onChange={value => updatePort(direction, port.name, {
                  type: value, file: value === "file" ? (port.file ?? {}) : undefined,
                })} options={typeNames.map(type => ({ value: type, label: type }))} />
                <TextField label="Purpose" value={port.description ?? ""}
                  onChange={value => updatePort(direction, port.name, { description: value || undefined })} />
                <CheckboxField label="Required" checked={port.required !== false}
                  onChange={checked => updatePort(direction, port.name, { required: checked ? undefined : false })} />
                <CheckboxField label="Nullable" checked={!!port.nullable}
                  onChange={checked => updatePort(direction, port.name, { nullable: checked || undefined })} />
                {port.type.endsWith("[]") && (
                  <>
                    <NumberField label="Minimum items" value={port.min_items} min={0}
                      onChange={value => updatePort(direction, port.name, { min_items: value })} />
                    <NumberField label="Maximum items" value={port.max_items} min={0}
                      onChange={value => updatePort(direction, port.name, { max_items: value })} />
                  </>
                )}
                {port.type === "file" && <TextField label="Media type" value={port.file?.media_type ?? ""}
                  onChange={value => updatePort(direction, port.name, { file: { ...port.file, media_type: value || undefined } })} />}
                <Button size="sm" variant="ghost" onClick={() => removePort(direction, port.name)}>Remove port</Button>
              </div>
            ))}
            <Button className="mt-2" size="sm" onClick={() => addPort(direction)}>Add {direction === "inputs" ? "input" : "output"}</Button>
          </section>
        ))}
        <section className="mt-4 border-t border-border-default pt-3">
          <h3 className="font-semibold text-fg-muted uppercase tracking-wide">Acceptance checks</h3>
          {(contract.criteria ?? []).map(criterion => (
            <div key={criterion.name} className="rounded border border-border-default p-2 mt-2">
              <p>{criterion.name}: {criterion.kind} on {criterion.port}</p>
              <Button size="sm" variant="ghost" onClick={() => save({ ...contract,
                criteria: (contract.criteria ?? []).filter(item => item.name !== criterion.name) })}>Remove check</Button>
            </div>
          ))}
          <Button size="sm" className="mt-2" onClick={() => {
            const name = uniqueName((contract.criteria ?? []).map(item => item.name), "min_length");
            const port = contract.outputs?.[0]?.name;
            if (port) save({ ...contract, criteria: [...(contract.criteria ?? []),
              { name, kind: "min_length", port: `output.${port}`, params: { min: 1 } }] });
          }} disabled={!contract.outputs?.length}>Add minimum-length check</Button>
        </section>
        <section className="mt-4 border-t border-border-default pt-3">
          <h3 className="font-semibold text-fg-muted uppercase tracking-wide">Declared effects</h3>
          {(contract.effects ?? []).map(effect => (
            <div key={effect.name} className="rounded border border-border-default p-2 mt-2">
              <CommittedTextField label="Effect name" value={effect.name} onChange={value => save({ ...contract,
                effects: (contract.effects ?? []).map(item => item.name === effect.name ? { ...item, name: value } : item) })} />
              <TextField label="Effect" value={effect.description} onChange={value => save({ ...contract,
                effects: (contract.effects ?? []).map(item => item.name === effect.name ? { ...item, description: value } : item) })} />
              <CheckboxField label="Paid operation" checked={!!effect.paid} onChange={value => save({ ...contract,
                effects: (contract.effects ?? []).map(item => item.name === effect.name ? { ...item, paid: value } : item) })} />
              <Button size="sm" variant="ghost" onClick={() => save({ ...contract,
                effects: (contract.effects ?? []).filter(item => item.name !== effect.name) })}>Remove effect</Button>
            </div>
          ))}
          <Button className="mt-2" size="sm" onClick={() => save({ ...contract, effects: [
            ...(contract.effects ?? []), { name: uniqueName((contract.effects ?? []).map(item => item.name), "effect"), description: "Describe the external operation" },
          ] })}>Add effect</Button>
        </section>
      </div>
    </details>
  );
}
