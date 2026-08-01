// ModelOverridesSection renders per-node model + backend pickers for the
// Launch form. It lets an operator re-target which provider/model/backend
// each LLM node (agent/judge) uses for THIS run, without editing the .bot —
// the studio surface of pkg/backend/model.ModelOverrides. LaunchView owns the
// override state and folds it into createRun()'s model_overrides.
//
// The selector sent per node is its exact node id, so the choice is precise
// (reviewer_claude vs reviewer_gpt). Nodes are grouped by kind (Judges,
// Agents) for scannability; a node left on "inherit" sends nothing, so the
// bot's DSL defaults apply unchanged.
//
// The model control reads the model registry (GET /api/models) rather than a
// datalist of the detected providers' suggested models: that older hint list
// could not say whether a spec was reachable, what it cost, or whether it
// could call tools at all. The nodes' own DSL defaults are passed to the
// registry as extra specs so a bot pinned outside the curated set still
// resolves — and so re-selecting the default stays one click.

import { useMemo, useState } from "react";

import type { BackendDetectReport } from "@/api/backends";
import type { ModelEntry } from "@/api/models";

import ModelPicker from "@/components/models/ModelPicker";
import { Select } from "@/components/ui/Select";
import { useModelCatalog } from "@/hooks/useModelCatalog";
import { nodeModelSpecs } from "@/lib/nodeModelSpecs";

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

function NodeRow({
  node,
  override,
  backendReport,
  models,
  recommended,
  onChange,
}: {
  node: LLMNode;
  override: NodeOverride;
  backendReport: BackendDetectReport | null;
  models: ModelEntry[];
  recommended: ModelEntry | null;
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
        <ModelPicker
          value={override.model ?? ""}
          onChange={(spec) => onChange({ model: spec })}
          models={models}
          recommended={recommended}
          compact
          inheritLabel={
            inheritModel ? `inherit — ${inheritModel}` : "inherit (bot default)"
          }
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
  const specs = useMemo(() => nodeModelSpecs(nodes), [nodes]);
  // Only fetch once the section is opened: the launch page mounts on every
  // navigation, and nobody needs the registry until they go looking for it.
  const { models, recommended, invalidSpecs, error } = useModelCatalog({
    extraSpecs: specs,
    enabled: open && nodes.length > 0,
  });

  if (nodes.length === 0) return null;

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
          {error && (
            <p className="text-caption text-warning">
              Could not load the model registry ({error}) — pick a model by
              typing its <code>provider/model-id</code>.
            </p>
          )}
          {invalidSpecs.length > 0 && (
            // A node's own `model:` that the registry cannot resolve is
            // skipped so it cannot blank the list — but skipping it in
            // silence turns "this bot pins a malformed spec" into "the
            // picker is missing my model".
            <p className="text-caption text-warning">
              {invalidSpecs.length === 1
                ? "One model pinned in this bot could not be resolved:"
                : `${invalidSpecs.length} models pinned in this bot could not be resolved:`}{" "}
              {invalidSpecs.map((s) => s.spec).join(", ")}. Fix the node&apos;s{" "}
              <code>model:</code> or override it here.
            </p>
          )}

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
                  models={models}
                  recommended={recommended}
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
                  models={models}
                  recommended={recommended}
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
