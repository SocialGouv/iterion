import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import type { AgentDecl, JudgeDecl } from "@/api/types";
import { defaultSchema, defaultPrompt } from "@/lib/defaults";
import { TextField, NumberField, SelectField, SelectFieldWithCreate, TagListField } from "./FormField";

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
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <TextField
        label="Model"
        value={decl.model}
        onChange={(v) => update({ model: v })}
        placeholder="e.g. ${ANTHROPIC_MODEL}"
      />
      <TextField
        label="Delegate"
        value={decl.delegate ?? ""}
        onChange={(v) => update({ delegate: v || undefined })}
        placeholder="e.g. claude_code"
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
      />
    </div>
  );
}
