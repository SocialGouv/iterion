import { useCallback, useMemo } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import type { RouterDecl, RouterMode } from "@/api/types";
import { getAllNodeNames, defaultPrompt } from "@/lib/defaults";
import { CommittedTextField, SelectField, SelectFieldWithCreate, TextField, CheckboxField } from "./FormField";
import { ProviderIcon, ProviderLabel } from "@/components/icons/ProviderIcon";
import { detectProvider } from "@/components/icons/providerDetect";

interface Props {
  decl: RouterDecl;
}

export default function RouterForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateRouter = useDocumentStore((s) => s.updateRouter);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const activeWorkflow = useActiveWorkflow();

  const outgoingEdges = useMemo(() => {
    if (!activeWorkflow) return [];
    return activeWorkflow.edges.filter((e) => e.from === decl.name);
  }, [activeWorkflow, decl.name]);

  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));

  const createPrompt = useCallback(() => {
    const existing = new Set((document?.prompts ?? []).map((p) => p.name));
    let i = 1;
    while (existing.has(`prompt_${i}`)) i++;
    const name = `prompt_${i}`;
    addPrompt(defaultPrompt(name));
    return name;
  }, [document, addPrompt]);

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
          {detectProvider(decl.model) && (
            <div className="flex items-center gap-1.5 px-2 py-1 mb-1 bg-surface-1/50 rounded text-[10px] text-fg-subtle">
              <ProviderIcon model={decl.model} size={14} />
              <span><ProviderLabel model={decl.model} /></span>
            </div>
          )}
          <TextField
            label="Model"
            value={decl.model ?? ""}
            onChange={(v) => updateRouter(decl.name, { model: v })}
            placeholder='e.g. ${ANTHROPIC_MODEL}'
            help="The LLM model to use for routing decisions (required)."
          />
          <SelectFieldWithCreate
            label="System Prompt"
            value={decl.system ?? ""}
            onChange={(v) => updateRouter(decl.name, { system: v })}
            options={promptOptions}
            allowEmpty
            emptyLabel="-- select prompt --"
            onCreate={createPrompt}
            help="Optional system prompt guiding the LLM's routing behavior."
          />
          <SelectFieldWithCreate
            label="User Prompt"
            value={decl.user ?? ""}
            onChange={(v) => updateRouter(decl.name, { user: v })}
            options={promptOptions}
            allowEmpty
            emptyLabel="-- select prompt --"
            onCreate={createPrompt}
            help="Optional user prompt template for the routing query."
          />
          <CheckboxField
            label="Multi (select multiple routes)"
            checked={decl.multi ?? false}
            onChange={(v) => updateRouter(decl.name, { multi: v })}
            help="When enabled, the LLM can select multiple routes for parallel execution."
          />
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
                <span className="text-amber-400 text-[10px]">
                  (when{e.when.negated ? " not" : ""} {e.when.condition})
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
    </div>
  );
}
