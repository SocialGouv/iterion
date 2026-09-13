import type { ContractDecl, PublicPortDecl } from "@/api/types";

function PortList({ title, ports }: { title: string; ports: PublicPortDecl[] }) {
  return (
    <section className="mt-3" aria-label={title}>
      <h3 className="text-xs font-semibold text-fg-muted uppercase tracking-wide">{title}</h3>
      {ports.length === 0 ? <p className="text-caption text-fg-subtle mt-1">None declared</p> : (
        <ul className="mt-1 space-y-1">
          {ports.map((port) => (
            <li key={port.name} className="rounded border border-border-default bg-surface-1 px-2 py-1.5 text-xs">
              <div className="flex items-center justify-between gap-2">
                <span className="font-semibold text-fg-default">{port.name}</span>
                <code className="text-fg-muted">{port.type}</code>
              </div>
              {port.description && <p className="text-fg-subtle mt-0.5">{port.description}</p>}
              <p className="text-caption text-fg-subtle mt-0.5">
                {port.required === false ? "optional" : "required"}
                {port.nullable ? " · nullable" : ""}
                {port.min_items !== undefined ? ` · min ${port.min_items}` : ""}
                {port.max_items !== undefined ? ` · max ${port.max_items}` : ""}
                {port.file ? ` · file${port.file.media_type ? ` (${port.file.media_type})` : ""}` : ""}
              </p>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** The public projection deliberately contains no prompts, tools or providers. */
export default function PublicContractPanel({ contract }: { contract: ContractDecl | undefined }) {
  if (!contract) return <p className="text-xs text-danger-fg">Public contract is missing.</p>;
  return (
    <div data-testid="public-contract">
      <h2 className="text-sm font-semibold text-fg-default">{contract.display_name || contract.name}</h2>
      <p className="text-caption text-fg-subtle">Contract {contract.name} · v{contract.version || 1}</p>
      <p className="text-xs text-fg-muted mt-2">{contract.responsibility}</p>
      <PortList title="Consumes" ports={contract.inputs ?? []} />
      <PortList title="Produces" ports={contract.outputs ?? []} />
      {(contract.criteria ?? []).length > 0 && (
        <section className="mt-3 text-xs">
          <h3 className="font-semibold text-fg-muted uppercase tracking-wide">Acceptance checks</h3>
          <ul className="mt-1 space-y-1">{contract.criteria!.map((criterion) => (
            <li key={criterion.name}>{criterion.name}: {criterion.kind} on {criterion.port}</li>
          ))}</ul>
        </section>
      )}
      {(contract.effects ?? []).length > 0 && (
        <section className="mt-3 text-xs">
          <h3 className="font-semibold text-fg-muted uppercase tracking-wide">Declared effects</h3>
          <ul className="mt-1 space-y-1">{contract.effects!.map((effect) => (
            <li key={effect.name}>{effect.name}{effect.paid ? " · paid" : ""}: {effect.description}</li>
          ))}</ul>
        </section>
      )}
    </div>
  );
}
