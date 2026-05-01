import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import type { AgentDecl, JudgeDecl, AwaitMode, InteractionMode, ReasoningEffort } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import {
  AWAIT_HELP,
  AWAIT_OPTIONS,
  BACKEND_HELP,
  BACKEND_OPTIONS,
  INTERACTION_HELP,
  INTERACTION_OPTIONS,
  REASONING_EFFORT_HELP,
  REASONING_EFFORT_OPTIONS,
  SESSION_HELP,
  SESSION_OPTIONS,
} from "@/lib/dslOptions";
import { TextField, CommittedTextField, NumberField, CheckboxField, SelectField, SelectFieldWithCreate, TagListField } from "./FormField";
import { ProviderIcon, ProviderLabel } from "@/components/icons/ProviderIcon";
import { detectProvider } from "@/components/icons/providerDetect";
import CompactionFields from "./CompactionFields";
import MCPConfigFields from "./MCPConfigFields";

interface Props {
  decl: AgentDecl | JudgeDecl;
  kind: "agent" | "judge";
}

export default function AgentForm({ decl, kind }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateAgent = useDocumentStore((s) => s.updateAgent);
  const updateJudge = useDocumentStore((s) => s.updateJudge);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const { createSchema, createPrompt } = useSchemaPromptCreators();

  const update = useCallback(
    (updates: Partial<AgentDecl>) => {
      if (kind === "agent") updateAgent(decl.name, updates);
      else updateJudge(decl.name, updates);
    },
    [kind, decl.name, updateAgent, updateJudge],
  );

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));

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
      {detectProvider(decl.model, decl.backend) && (
        <div className="flex items-center gap-1.5 px-2 py-1 mb-1 bg-surface-1/50 rounded text-[10px] text-fg-subtle">
          <ProviderIcon model={decl.model} delegate={decl.backend} size={14} />
          <span><ProviderLabel model={decl.model} delegate={decl.backend} /></span>
        </div>
      )}
      <TextField
        label="Model"
        value={decl.model}
        onChange={(v) => update({ model: v })}
        placeholder="e.g. ${ANTHROPIC_MODEL}"
        help="LLM model identifier. Use ${ENV_VAR} for environment variable substitution. Required unless backend is set."
      />
      <SelectField
        label="Backend"
        value={decl.backend ?? ""}
        onChange={(v) => update({ backend: v || undefined })}
        options={BACKEND_OPTIONS}
        help={BACKEND_HELP}
      />
      <NumberField
        label="Max Tokens"
        value={decl.max_tokens}
        onChange={(v) => update({ max_tokens: v || undefined })}
        min={1}
        help="Per-LLM-call output cap. 0/empty inherits the backend default."
      />
      <SelectField
        label="Reasoning Effort"
        value={decl.reasoning_effort ?? ""}
        onChange={(v) => update({ reasoning_effort: (v || undefined) as ReasoningEffort | undefined })}
        options={REASONING_EFFORT_OPTIONS}
        help={REASONING_EFFORT_HELP}
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
        options={SESSION_OPTIONS}
        help={SESSION_HELP}
      />
      <SelectField
        label="Await"
        value={decl.await ?? "none"}
        onChange={(v) => update({ await: (v === "none" ? undefined : v) as AwaitMode | undefined })}
        options={AWAIT_OPTIONS}
        help={AWAIT_HELP}
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
      <TagListField
        label="Tool Policy"
        values={decl.tool_policy ?? []}
        onChange={(v) => update({ tool_policy: v.length > 0 ? v : undefined })}
        placeholder="Add allow/deny pattern..."
      />
      <CheckboxField
        label="Readonly"
        checked={!!decl.readonly}
        onChange={(v) => update({ readonly: v || undefined })}
        help="When true, the runtime treats this node as non-mutating — multiple readonly branches can run concurrently."
      />
      <SelectField
        label="Interaction"
        value={decl.interaction ?? "none"}
        onChange={(v) =>
          update({ interaction: (v === "none" ? undefined : v) as InteractionMode | undefined })
        }
        options={INTERACTION_OPTIONS}
        help={INTERACTION_HELP}
      />
      {decl.interaction === "llm" || decl.interaction === "llm_or_human" ? (
        <>
          <SelectFieldWithCreate
            label="Interaction Prompt"
            value={decl.interaction_prompt ?? ""}
            onChange={(v) => update({ interaction_prompt: v || undefined })}
            options={promptOptions}
            allowEmpty
            emptyLabel="-- select prompt --"
            onCreate={createPrompt}
          />
          <TextField
            label="Interaction Model"
            value={decl.interaction_model ?? ""}
            onChange={(v) => update({ interaction_model: v || undefined })}
            placeholder="(falls back to Model)"
          />
        </>
      ) : null}
      <CompactionFields
        value={decl.compaction}
        onChange={(c) => update({ compaction: c })}
      />
      <MCPConfigFields
        scope="node"
        value={decl.mcp}
        onChange={(c) => update({ mcp: c })}
      />
    </div>
  );
}
