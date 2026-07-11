import { useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import { effortBackendKey, useEffortCapabilities } from "@/hooks/useEffortCapabilities";
import { usePromptEditorMount } from "@/hooks/usePromptEditorMount";
import type { ReasoningEffort, RouterDecl, RouterMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import {
  BACKEND_HELP,
  BACKEND_OPTIONS,
  REASONING_EFFORT_HELP,
  REASONING_EFFORT_OPTIONS,
} from "@/lib/dslOptions";
import { NODE_COLORS } from "@/lib/constants";
import { CommittedTextField, SelectField, TextField, CheckboxField, PromptPickerField, NodeFormHeader } from "./FormField";
import { ProviderIcon, ProviderLabel } from "@/components/icons/ProviderIcon";
import { detectProvider } from "@/components/icons/providerDetect";

interface Props {
  decl: RouterDecl;
}

export default function RouterForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateRouter = useDocumentStore((s) => s.updateRouter);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const activeWorkflow = useActiveWorkflow();
  const { createPrompt } = useSchemaPromptCreators();

  const outgoingEdges = useMemo(() => {
    if (!activeWorkflow) return [];
    return activeWorkflow.edges.filter((e) => e.from === decl.name);
  }, [activeWorkflow, decl.name]);

  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));
  const { openPromptEditor, lookupBody, promptModal } = usePromptEditorMount();

  // Mirror AgentForm: derive effort options from the model registry when
  // mode=llm, falling back to the static list while loading or when the
  // backend/model is empty.
  const effortBackend = effortBackendKey(decl.backend);
  const { capabilities: effortCaps, loading: effortLoading } = useEffortCapabilities(
    decl.mode === "llm" ? effortBackend : undefined,
    decl.mode === "llm" ? decl.model : undefined,
  );
  // Mirror AgentForm: surface env-substituted literals as a synthetic
  // dropdown option so the native <select> doesn't fall through to the
  // placeholder ("default") for values like "${VAR:-max}".
  const envSubstEffort =
    decl.reasoning_effort && decl.reasoning_effort.includes("$")
      ? decl.reasoning_effort
      : null;
  const effortOptions = useMemo(() => {
    let base: { value: string; label: string }[];
    if (effortLoading || !effortCaps) {
      base = REASONING_EFFORT_OPTIONS;
    } else {
      const supported = effortCaps.supported ?? [];
      base =
        supported.length === 0
          ? []
          : [
              {
                value: "",
                label: effortCaps.default
                  ? `(default: ${effortCaps.default})`
                  : "(default)",
              },
              ...supported.map((s) => ({ value: s, label: s })),
            ];
    }
    if (envSubstEffort) {
      return [{ value: envSubstEffort, label: envSubstEffort }, ...base];
    }
    return base;
  }, [effortCaps, effortLoading, envSubstEffort]);
  const effortNotSupported =
    !effortLoading &&
    effortCaps !== null &&
    (effortCaps.supported ?? []).length === 0;
  const effortHelp = useMemo(() => {
    if (envSubstEffort) {
      return `Env-substituted at runtime. Pick a literal level to drop the substitution.`;
    }
    if (effortLoading || !effortCaps) return REASONING_EFFORT_HELP;
    if (effortNotSupported) {
      return `${decl.model || "This model"} does not support reasoning_effort.`;
    }
    return `${REASONING_EFFORT_HELP} Levels available for ${decl.model}: ${(effortCaps.supported ?? []).join(", ")}.`;
  }, [effortLoading, effortCaps, effortNotSupported, decl.model, envSubstEffort]);

  const headerColor = NODE_COLORS.router;

  return (
    <div className="space-y-1">
      <NodeFormHeader color={headerColor} icon={"\u{1F504}"} label="Router" />
      <CommittedTextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
        onCommit={(v) => setSelectedNode(v)}
        validate={(v) => {
          if (!v.trim()) return "Name cannot be empty";
          if (/\s/.test(v)) return "Name cannot contain spaces";
          const names = getAllNodeNames(document!);
          names.delete(decl.name);
          if (names.has(v)) return "Name already exists";
          return null;
        }}
      />
      <SelectField
        label="Mode"
        value={decl.mode}
        onChange={(v) => updateRouter(decl.name, { mode: v as RouterMode })}
        options={[
          { value: "fan_out_all", label: "fan_out_all" },
          { value: "fan_out_each", label: "fan_out_each" },
          { value: "condition", label: "condition" },
          { value: "round_robin", label: "round_robin" },
          { value: "llm", label: "llm" },
        ]}
        help="fan_out_all = send to all targets in parallel; fan_out_each = re-run the outgoing subgraph once per element of a runtime array (with optional DAG scheduling); condition = route on 'when' clauses; round_robin = cycle through targets; llm = LLM selects route(s)."
      />
      {decl.mode === "llm" && (
        <div className="mt-2 space-y-1">
          {detectProvider(decl.model, decl.backend) && (
            <div className="flex items-center gap-1.5 px-2 py-1 mb-1 bg-surface-1/50 rounded text-caption text-fg-subtle">
              <ProviderIcon model={decl.model} delegate={decl.backend} size={14} />
              <span><ProviderLabel model={decl.model} delegate={decl.backend} /></span>
            </div>
          )}
          <TextField
            label="Model"
            value={decl.model ?? ""}
            onChange={(v) => updateRouter(decl.name, { model: v })}
            placeholder='e.g. ${ANTHROPIC_MODEL}'
            help="The LLM model to use for routing decisions (required)."
          />
          <SelectField
            label="Backend"
            value={decl.backend ?? ""}
            onChange={(v) => updateRouter(decl.name, { backend: v || undefined })}
            options={BACKEND_OPTIONS}
            help={BACKEND_HELP}
          />
          <PromptPickerField
            label="System Prompt"
            value={decl.system ?? ""}
            onChange={(v) => updateRouter(decl.name, { system: v })}
            options={promptOptions}
            onCreate={createPrompt}
            onEdit={openPromptEditor}
            body={lookupBody(decl.system)}
            allowEmpty
            help="Optional system prompt guiding the LLM's routing behavior."
          />
          <PromptPickerField
            label="User Prompt"
            value={decl.user ?? ""}
            onChange={(v) => updateRouter(decl.name, { user: v })}
            options={promptOptions}
            onCreate={createPrompt}
            onEdit={openPromptEditor}
            body={lookupBody(decl.user)}
            allowEmpty
            help="Optional user prompt template for the routing query."
          />
          <CheckboxField
            label="Multi (select multiple routes)"
            checked={decl.multi ?? false}
            onChange={(v) => updateRouter(decl.name, { multi: v })}
            help="When enabled, the LLM can select multiple routes for parallel execution."
          />
          {(!effortNotSupported || decl.reasoning_effort) && (
            <SelectField
              label="Reasoning Effort"
              value={decl.reasoning_effort ?? ""}
              onChange={(v) =>
                updateRouter(decl.name, {
                  reasoning_effort: (v || undefined) as ReasoningEffort | undefined,
                })
              }
              options={effortOptions}
              help={effortHelp}
            />
          )}
        </div>
      )}
      {decl.mode === "condition" && (
        <div className="mt-2 p-2 bg-surface-1 rounded border border-border-default">
          <p className="text-caption text-fg-subtle mb-2">
            In condition mode, routing is controlled by &quot;when&quot; clauses on outgoing edges. Click an edge to add conditions.
          </p>
          {outgoingEdges.length === 0 && (
            <p className="text-caption text-warning">No outgoing edges yet. Connect this router to target nodes.</p>
          )}
          {outgoingEdges.map((e, i) => (
            <div key={i} className="text-xs text-fg-muted flex items-center gap-1 py-0.5">
              <span className="text-fg-subtle">&rarr;</span>
              <span>{e.to}</span>
              {e.when ? (
                <span className="text-warning-fg text-caption">
                  {e.when.expr
                    ? `(when ${e.when.expr.length > 24 ? e.when.expr.slice(0, 24) + "…" : e.when.expr})`
                    : `(when${e.when.negated ? " not" : ""} ${e.when.condition})`}
                </span>
              ) : (
                <span className="text-fg-subtle text-caption">(no condition)</span>
              )}
            </div>
          ))}
        </div>
      )}
      {decl.mode === "fan_out_all" && outgoingEdges.length > 0 && (
        <p className="text-caption text-fg-subtle mt-1">
          Sends input to {outgoingEdges.length} target{outgoingEdges.length !== 1 ? "s" : ""} in parallel.
        </p>
      )}
      {decl.mode === "fan_out_each" && (
        <div className="mt-2 space-y-1">
          <TextField
            label="Over (array source)"
            value={decl.over ?? ""}
            onChange={(v) => updateRouter(decl.name, { over: v })}
            placeholder="{{outputs.decompose.tickets}}"
            help="Template resolving to a runtime array. The single outgoing edge's subgraph runs once per element."
          />
          <TextField
            label="Item binding (as)"
            value={decl.as ?? ""}
            onChange={(v) => updateRouter(decl.name, { as: v || undefined })}
            placeholder="item"
            help="Name the element is bound to (default: item). Reference as {{outputs.<router>.<as>.<field>}}."
          />
          <TextField
            label="Key field (enables DAG)"
            value={decl.key ?? ""}
            onChange={(v) => updateRouter(decl.name, { key: v || undefined })}
            placeholder="id"
            help="Optional: item field holding each element's unique id. Setting it enables dependency-DAG scheduling."
          />
          <TextField
            label="Depends-on field"
            value={decl.depends_on ?? ""}
            onChange={(v) => updateRouter(decl.name, { depends_on: v || undefined })}
            placeholder="deps"
            help="Optional: item field holding the ids it depends on. Independent items run in parallel (bounded by Max Parallel Branches)."
          />
          {outgoingEdges.length !== 1 && (
            <p className="text-caption text-warning mt-1">
              fan_out_each needs exactly one outgoing edge (the per-item template head); this router has {outgoingEdges.length}.
            </p>
          )}
        </div>
      )}
      {decl.mode === "round_robin" && outgoingEdges.length > 0 && (
        <p className="text-caption text-fg-subtle mt-1">
          Cycles through {outgoingEdges.length} target{outgoingEdges.length !== 1 ? "s" : ""} one at a time.
        </p>
      )}
      {promptModal}
    </div>
  );
}
