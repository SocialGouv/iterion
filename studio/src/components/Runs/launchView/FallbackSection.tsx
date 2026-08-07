// FallbackSection renders the Launch form's single run-level fallback
// route: "if an agent exhausts its quota, continue on <backend> <model>".
//
// It is deliberately ONE row rather than an ordered list per node. The
// value operators actually want is "don't lose a 40-minute run to a
// forfait wall", which one alternative delivers; a per-node chain
// multiplied by chain length is unusable on a 15-node bot, and this
// form persists nothing between launches, so every cell would be
// rebuilt each time. Authoring a real chain belongs in the .bot's
// `fallbacks:` block, which is per-node and ordered.
//
// Two scoping rules are surfaced in the caption rather than hidden:
// the route never reaches a judge (a weaker judge still emits a
// well-formed verdict — only the finding count changes, which a
// deterministic merge gate reads), and a node that declares its own
// `fallbacks:` keeps them. See ADR-087.

import type { BackendDetectReport } from "@/api/backends";

import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

export interface FallbackSectionProps {
  backend: string;
  model: string;
  backendReport: BackendDetectReport | null;
  /** True when the server runs in cloud mode. The detect report then
   *  describes the SERVER host, while the run executes on a runner pod
   *  with tenant-injected credentials — so availability is advisory
   *  there, never a reason to disable a choice. */
  cloud: boolean;
  onBackendChange: (value: string) => void;
  onModelChange: (value: string) => void;
}

export default function FallbackSection({
  backend,
  model,
  backendReport,
  cloud,
  onBackendChange,
  onModelChange,
}: FallbackSectionProps) {
  const suggestions = (backendReport?.providers ?? [])
    .filter((p) => p.available && p.suggested_model)
    .map((p) => p.suggested_model as string);

  return (
    <div className="space-y-2 border-t border-border-default pt-3">
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-sm font-medium">Fallback route</span>
        <span className="text-xs text-fg-muted">off unless set</span>
      </div>
      <p className="text-xs text-fg-muted">
        If an agent&apos;s quota window shuts or its model is unreachable, continue
        there instead of failing the run. Judges are never re-routed, and a node
        with its own <code>fallbacks:</code> keeps them.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <Select
          aria-label="Fallback backend"
          value={backend}
          onChange={(e) => onBackendChange(e.target.value)}
        >
          <option value="">no fallback</option>
          {(backendReport?.backends ?? []).map((b) => (
            <option
              key={b.name}
              value={b.name}
              disabled={!cloud && !b.available}
            >
              {b.name}
              {!cloud && !b.available ? " (no credential)" : ""}
            </option>
          ))}
        </Select>
        <Input
          aria-label="Fallback model"
          className="min-w-56 flex-1"
          list="fallback-model-suggestions"
          placeholder="model (e.g. openai/gpt-5.5)"
          value={model}
          disabled={backend === ""}
          onChange={(e) => onModelChange(e.target.value)}
        />
        <datalist id="fallback-model-suggestions">
          {[...new Set(suggestions)].sort().map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
      </div>
      {backend !== "" && model.trim() === "" && (
        <p className="text-xs text-warning-fg">
          A route that changes backend needs its own model — model specs are not
          portable between backends.
        </p>
      )}
    </div>
  );
}
