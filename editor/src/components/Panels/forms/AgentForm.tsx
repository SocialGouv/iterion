import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import type { AgentDecl, JudgeDecl } from "@/api/types";
import { defaultSchema, defaultPrompt, getAllNodeNames } from "@/lib/defaults";
import { TextField, CommittedTextField, NumberField, SelectField, SelectFieldWithCreate, TagListField } from "./FormField";
import { ProviderIcon, ProviderLabel } from "@/components/icons/ProviderIcon";
import { detectProvider } from "@/components/icons/providerDetect";

interface Props {
  decl: AgentDecl | JudgeDecl;
  kind: "agent" | "judge";
}

export default function AgentForm({ decl, kind }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateAgent = useDocumentStore((s) => s.updateAgent);
  const updateJudge = useDocumentStore((s) => s.updateJudge);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const update = useCallback(
    (updates: Partial<AgentDecl>) => {
      if (kind === "agent") updateAgent(decl.name, updates);
      else updateJudge(decl.name, updates);
    },
    [kind, decl.name, updateAgent, updateJudge],
  );

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));

  const createSchema = useCallback(() => {
    const existing = new Set((document?.schemas ?? []).map((s) => s.name));
    let i = 1;
    while (existing.has(`schema_${i}`)) i++;
    const name = `schema_${i}`;
    addSchema(defaultSchema(name));
    return name;
  }, [document, addSchema]);

  const createPrompt = useCallback(() => {
    const existing = new Set((document?.prompts ?? []).map((p) => p.name));
    let i = 1;
    while (existing.has(`prompt_${i}`)) i++;
    const name = `prompt_${i}`;
    addPrompt(defaultPrompt(name));
    return name;
  }, [document, addPrompt]);

  const headerColor = kind === "agent" ? "#4A90D9" : "#7B68EE";
  const headerIcon = kind === "agent" ? "\u{1F916}" : "\u{2696}\u{FE0F}";
  const headerLabel = kind === "agent" ? "Agent" : "Judge";

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: headerColor + "22", borderLeft: `3px solid ${headerColor}` }}
      >
        <span className="text-base">{headerIcon}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: headerColor }}>{headerLabel}</span>
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
      {detectProvider(decl.model, decl.delegate) && (
        <div className="flex items-center gap-1.5 px-2 py-1 mb-1 bg-gray-800/50 rounded text-[10px] text-gray-400">
          <ProviderIcon model={decl.model} delegate={decl.delegate} size={14} />
          <span><ProviderLabel model={decl.model} delegate={decl.delegate} /></span>
        </div>
      )}
      <TextField
        label="Model"
        value={decl.model}
        onChange={(v) => update({ model: v })}
        placeholder="e.g. ${ANTHROPIC_MODEL}"
        help="LLM model identifier. Use ${ENV_VAR} for environment variable substitution. Required unless delegate is set."
      />
      <TextField
        label="Delegate"
        value={decl.delegate ?? ""}
        onChange={(v) => update({ delegate: v || undefined })}
        placeholder="e.g. claude_code"
        help="Delegate execution to an external backend (e.g. claude_code, codex) instead of calling the model API directly."
      />
      <SelectFieldWithCreate
        label="Input Schema"
        value={decl.input}
        onChange={(v) => update({ input: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => update({ output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
      <TextField
        label="Publish"
        value={decl.publish ?? ""}
        onChange={(v) => update({ publish: v || undefined })}
        placeholder="Artifact name"
        help="Publish this node's output as a persistent artifact, accessible downstream via {{artifacts.name}}."
      />
      <SelectFieldWithCreate
        label="System Prompt"
        value={decl.system}
        onChange={(v) => update({ system: v })}
        options={promptOptions}
        allowEmpty
        emptyLabel="-- select prompt --"
        onCreate={createPrompt}
      />
      <SelectFieldWithCreate
        label="User Prompt"
        value={decl.user}
        onChange={(v) => update({ user: v })}
        options={promptOptions}
        allowEmpty
        emptyLabel="-- select prompt --"
        onCreate={createPrompt}
      />
      <SelectField
        label="Session"
        value={decl.session}
        onChange={(v) => update({ session: v as AgentDecl["session"] })}
        options={[
          { value: "fresh", label: "fresh" },
          { value: "inherit", label: "inherit" },
          { value: "artifacts_only", label: "artifacts_only" },
        ]}
        help="fresh = new context; inherit = reuse parent conversation; artifacts_only = share published artifacts only. Cannot use inherit after a join node."
      />
      <TagListField
        label="Tools"
        values={decl.tools ?? []}
        onChange={(v) => update({ tools: v.length > 0 ? v : undefined })}
        placeholder="Add tool..."
      />
      <NumberField
        label="Tool Max Steps"
        value={decl.tool_max_steps}
        onChange={(v) => update({ tool_max_steps: v })}
        min={1}
        help="Maximum number of tool-use iterations the agent can perform before returning."
      />
    </div>
  );
}
