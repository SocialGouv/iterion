// ModelOverridesSection renders per-node model + backend dropdowns for the
// Launch form. It lets an operator re-target which provider/model/backend
// each LLM node (agent/judge) uses for THIS run, without editing the .bot —
// the studio surface of pkg/backend/model.ModelOverrides. LaunchView owns the
// override state and folds it into createRun()'s model_overrides.
//
// The selector sent per node is its exact node id, so the choice is precise
// (reviewer_claude vs reviewer_gpt). Nodes are grouped by kind (Judges,
// Agents) for scannability; a node left on "inherit" sends nothing, so the
// bot's DSL defaults apply unchanged.

import { useState } from "react";

import type { BackendDetectReport } from "@/api/backends";

import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

// One LLM node the operator can retarget.
export interface LLMNode {
  name: string;
  kind: "agent" | "judge";
  model: string; // the node's DSL default (may be "" or a ${VAR} literal)
  backend?: string; // the node's DSL default backend, if pinned
}

// Per-node override the operator has set (empty fields = inherit).
export interface NodeOverride {
  model?: string;
  backend?: string;
}

export interface ModelOverridesSectionProps {
  nodes: LLMNode[];
  overrides: Record<string, NodeOverride>;
  backendReport: BackendDetectReport | null;
  onChange: (nodeName: string, patch: NodeOverride) => void;
}

// modelSuggestions collects distinct model specs to offer in the datalist:
// every detected provider's suggested model, plus the nodes' own DSL defaults
// (so re-selecting the default is one keystroke). Literal ${VAR} defaults are
// skipped — they aren't real model ids.
function modelSuggestions(
  nodes: LLMNode[],
  report: BackendDetectReport | null,
): string[] {
  const set = new Set<string>();
  for (const p of report?.providers ?? []) {
    if (p.available && p.suggested_model) set.add(p.suggested_model);
  }
  for (const n of nodes) {
    if (n.model && !n.model.includes("${")) set.add(n.model);
  }
  return [...set].sort();
}

function NodeRow({
  node,
  override,
  backendReport,
  suggestionsId,
  onChange,
}: {
  node: LLMNode;
  override: NodeOverride;
  backendReport: BackendDetectReport | null;
  suggestionsId: string;
  onChange: (patch: NodeOverride) => void;
}) {
  const inheritModel = node.model && !node.model.includes("${") ? node.model : "";
  const backends = backendReport?.backends ?? [];
  return (
    <div className="grid grid-cols-[160px_1fr_140px] gap-3 items-start">
      <div className="min-w-0">
        <div className="text-xs font-medium font-mono truncate" title={node.name}>
          {node.name}
        </div>
        <div className="text-caption text-fg-subtle">{node.kind}</div>
      </div>
      <div>
        <Input
          size="sm"
          type="text"
          list={suggestionsId}
          className="font-mono"
          placeholder={inheritModel ? `inherit — ${inheritModel}` : "inherit (bot default)"}
          value={override.model ?? ""}
          onChange={(e) => onChange({ model: e.currentTarget.value })}
        />
      </div>
      <div>
        <Select
          value={override.backend ?? ""}
          onChange={(e) => onChange({ backend: e.currentTarget.value })}
        >
          <option value="">
            inherit{node.backend ? ` — ${node.backend}` : ""}
          </option>
          {backends.map((b) => (
            <option key={b.name} value={b.name} disabled={!b.available}>
              {b.name}
              {b.available ? "" : " — no credential"}
            </option>
          ))}
        </Select>
      </div>
    </div>
  );
}

export default function ModelOverridesSection({
  nodes,
  overrides,
  backendReport,
  onChange,
}: ModelOverridesSectionProps) {
  const [open, setOpen] = useState(false);
  if (nodes.length === 0) return null;

  const suggestionsId = "iterion-model-suggestions";
  const suggestions = modelSuggestions(nodes, backendReport);
  const setCount = Object.values(overrides).filter(
    (o) => o.model || o.backend,
  ).length;

  const judges = nodes.filter((n) => n.kind === "judge");
  const agents = nodes.filter((n) => n.kind === "agent");

  return (
    <section className="mt-6 border-t border-border-default pt-4 mb-6">
      <button
        type="button"
        className="flex items-center gap-2 text-xs font-medium text-fg-muted hover:text-fg-default"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span>{open ? "▾" : "▸"}</span>
        <span>Model &amp; backend per node</span>
        {setCount > 0 && (
          <span className="rounded-full bg-accent-soft px-1.5 py-0.5 text-caption text-accent-fg">
            {setCount} overridden
          </span>
        )}
      </button>

      {open && (
        <div className="mt-3 space-y-4">
          <p className="text-caption text-fg-subtle">
            Retarget which provider/model or backend each node uses for this run
            — leave a field on <em>inherit</em> to keep the bot&apos;s default.
            These win over the node&apos;s <code>model:</code>/<code>backend:</code>{" "}
            and compose with the review-mode (mono/dual) topology.
          </p>
          <datalist id={suggestionsId}>
            {suggestions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>

          {judges.length > 0 && (
            <div className="space-y-2">
              <div className="text-caption font-medium text-fg-muted uppercase tracking-wide">
                Judges (review / verdict)
              </div>
              {judges.map((n) => (
                <NodeRow
                  key={n.name}
                  node={n}
                  override={overrides[n.name] ?? {}}
                  backendReport={backendReport}
                  suggestionsId={suggestionsId}
                  onChange={(patch) => onChange(n.name, patch)}
                />
              ))}
            </div>
          )}

          {agents.length > 0 && (
            <div className="space-y-2">
              <div className="text-caption font-medium text-fg-muted uppercase tracking-wide">
                Agents (implement / fix)
              </div>
              {agents.map((n) => (
                <NodeRow
                  key={n.name}
                  node={n}
                  override={overrides[n.name] ?? {}}
                  backendReport={backendReport}
                  suggestionsId={suggestionsId}
                  onChange={(patch) => onChange(n.name, patch)}
                />
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  );
}
