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
import { CommittedTextField, SelectField, TextField, CheckboxField, PromptPickerField } from "./FormField";
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
  const effortOptions = useMemo(() => {
    if (effortLoading || !effortCaps) return REASONING_EFFORT_OPTIONS;
    const supported = effortCaps.supported ?? [];
    if (supported.length === 0) return [];
    const defaultLabel = effortCaps.default ? `(default: ${effortCaps.default})` : "(default)";
    return [
      { value: "", label: defaultLabel },
      ...supported.map((s) => ({ value: s, label: s })),
    ];
  }, [effortCaps, effortLoading]);
  const effortNotSupported =
    !effortLoading &&
    effortCaps !== null &&
    (effortCaps.supported ?? []).length === 0;
  const effortHelp = useMemo(() => {
    if (effortLoading || !effortCaps) return REASONING_EFFORT_HELP;
    if (effortNotSupported) {
      return `${decl.model || "This model"} does not support reasoning_effort.`;
    }
    return `${REASONING_EFFORT_HELP} Levels available for ${decl.model}: ${(effortCaps.supported ?? []).join(", ")}.`;
  }, [effortLoading, effortCaps, effortNotSupported, decl.model]);

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#E67E2222", borderLeft: "3px solid #E67E22" }}
      >
        <span className="text-base">{"\u{1F504}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#E67E22" }}>Router</span>
      </div>
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
          { value: "condition", label: "condition" },
          { value: "round_robin", label: "round_robin" },
          { value: "llm", label: "llm" },
        ]}
        help="fan_out_all = send to all targets in parallel; condition = route on 'when' clauses; round_robin = cycle through targets; llm = LLM selects route(s)."
      />
      {decl.mode === "llm" && (
        <div className="mt-2 space-y-1">
          {detectProvider(decl.model, decl.backend) && (
            <div className="flex items-center gap-1.5 px-2 py-1 mb-1 bg-surface-1/50 rounded text-[10px] text-fg-subtle">
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
          <p className="text-[10px] text-fg-subtle mb-2">
            In condition mode, routing is controlled by &quot;when&quot; clauses on outgoing edges. Click an edge to add conditions.
          </p>
          {outgoingEdges.length === 0 && (
            <p className="text-[10px] text-warning">No outgoing edges yet. Connect this router to target nodes.</p>
          )}
          {outgoingEdges.map((e, i) => (
            <div key={i} className="text-xs text-fg-muted flex items-center gap-1 py-0.5">
              <span className="text-fg-subtle">&rarr;</span>
              <span>{e.to}</span>
              {e.when ? (
                <span className="text-warning-fg text-[10px]">
                  {e.when.expr
                    ? `(when ${e.when.expr.length > 24 ? e.when.expr.slice(0, 24) + "…" : e.when.expr})`
                    : `(when${e.when.negated ? " not" : ""} ${e.when.condition})`}
                </span>
              ) : (
                <span className="text-fg-subtle text-[10px]">(no condition)</span>
              )}
            </div>
          ))}
        </div>
      )}
      {decl.mode === "fan_out_all" && outgoingEdges.length > 0 && (
        <p className="text-[10px] text-fg-subtle mt-1">
          Sends input to {outgoingEdges.length} target{outgoingEdges.length !== 1 ? "s" : ""} in parallel.
        </p>
      )}
      {decl.mode === "round_robin" && outgoingEdges.length > 0 && (
        <p className="text-[10px] text-fg-subtle mt-1">
          Cycles through {outgoingEdges.length} target{outgoingEdges.length !== 1 ? "s" : ""} one at a time.
        </p>
      )}
      {promptModal}
    </div>
  );
}
