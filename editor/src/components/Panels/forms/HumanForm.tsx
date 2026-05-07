import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useSchemaPromptCreators } from "@/hooks/useSchemaPromptCreators";
import { usePromptEditorMount } from "@/hooks/usePromptEditorMount";
import type { HumanDecl, InteractionMode, AwaitMode } from "@/api/types";
import { getAllNodeNames } from "@/lib/defaults";
import {
  AWAIT_HELP,
  AWAIT_OPTIONS,
  HUMAN_INTERACTION_HELP,
  HUMAN_INTERACTION_OPTIONS,
} from "@/lib/dslOptions";
import { TextField, CommittedTextField, NumberField, SelectField, SelectFieldWithCreate, PromptPickerField } from "./FormField";
import { NODE_COLORS } from "@/lib/constants";
import { NodeIcon } from "@/components/icons/NodeIcon";

interface Props {
  decl: HumanDecl;
}

export default function HumanForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateHuman = useDocumentStore((s) => s.updateHuman);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const { createSchema, createPrompt } = useSchemaPromptCreators();

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));
  const { openPromptEditor, lookupBody, promptModal } = usePromptEditorMount();

  // The unified InteractionMode replaces the legacy editor-only field.
  // human-only modes from the old UI map onto:
  //   pause_until_answers → "human"
  //   auto_answer         → "llm"
  //   auto_or_pause       → "llm_or_human"
  const interaction: InteractionMode = decl.interaction ?? "human";
  const needsModel = interaction === "llm" || interaction === "llm_or_human";

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: `${NODE_COLORS.human}22`, borderLeft: `3px solid ${NODE_COLORS.human}` }}
      >
        <NodeIcon kind="human" size={16} />
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: NODE_COLORS.human }}>Human</span>
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
      <SelectFieldWithCreate
        label="Input Schema"
        value={decl.input}
        onChange={(v) => updateHuman(decl.name, { input: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateHuman(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
      <TextField
        label="Publish"
        value={decl.publish ?? ""}
        onChange={(v) => updateHuman(decl.name, { publish: v || undefined })}
        placeholder="Artifact name"
        help="Publish this node's output as a persistent artifact, accessible downstream via {{artifacts.name}}."
      />
      <PromptPickerField
        label="Instructions Prompt"
        value={decl.instructions}
        onChange={(v) => updateHuman(decl.name, { instructions: v })}
        options={promptOptions}
        onCreate={createPrompt}
        onEdit={openPromptEditor}
        body={lookupBody(decl.instructions)}
        allowEmpty
      />
      <SelectField
        label="Interaction"
        value={interaction}
        onChange={(v) => updateHuman(decl.name, { interaction: v as InteractionMode })}
        options={HUMAN_INTERACTION_OPTIONS}
        help={HUMAN_INTERACTION_HELP}
      />
      {needsModel && (
        <>
          <PromptPickerField
            label="Interaction Prompt"
            value={decl.interaction_prompt ?? ""}
            onChange={(v) => updateHuman(decl.name, { interaction_prompt: v || undefined })}
            options={promptOptions}
            onCreate={createPrompt}
            onEdit={openPromptEditor}
            body={lookupBody(decl.interaction_prompt)}
            allowEmpty
          />
          <TextField
            label="Interaction Model"
            value={decl.interaction_model ?? ""}
            onChange={(v) => updateHuman(decl.name, { interaction_model: v || undefined })}
            placeholder="(falls back to Model below)"
          />
        </>
      )}
      <NumberField
        label="Min Answers"
        value={decl.min_answers}
        onChange={(v) => updateHuman(decl.name, { min_answers: v })}
        min={1}
      />
      <SelectField
        label="Await"
        value={decl.await ?? "none"}
        onChange={(v) => updateHuman(decl.name, { await: (v === "none" ? undefined : v) as AwaitMode | undefined })}
        options={AWAIT_OPTIONS}
        help={AWAIT_HELP}
      />
      {needsModel && (
        <div className="border-t border-border-default pt-2 mt-2">
          <p className="text-[10px] text-warning mb-1">Required for {interaction} interaction</p>
        </div>
      )}
      <TextField
        label="Model"
        value={decl.model ?? ""}
        onChange={(v) => updateHuman(decl.name, { model: v || undefined })}
        placeholder={needsModel ? "e.g. ${ANTHROPIC_MODEL}" : "Optional model override"}
      />
      <PromptPickerField
        label="System Prompt"
        value={decl.system ?? ""}
        onChange={(v) => updateHuman(decl.name, { system: v || undefined })}
        options={promptOptions}
        onCreate={createPrompt}
        onEdit={openPromptEditor}
        body={lookupBody(decl.system)}
        allowEmpty
      />
      {promptModal}
    </div>
  );
}
