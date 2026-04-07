import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import type { HumanDecl, HumanMode, AwaitMode } from "@/api/types";
import { defaultSchema, defaultPrompt, getAllNodeNames } from "@/lib/defaults";
import { TextField, CommittedTextField, NumberField, SelectField, SelectFieldWithCreate } from "./FormField";

interface Props {
  decl: HumanDecl;
}

export default function HumanForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateHuman = useDocumentStore((s) => s.updateHuman);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const addPrompt = useDocumentStore((s) => s.addPrompt);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

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

  const needsModel = decl.mode === "auto_answer" || decl.mode === "auto_or_pause";

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#E74C3C22", borderLeft: "3px solid #E74C3C" }}
      >
        <span className="text-base">{"\u{1F464}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#E74C3C" }}>Human</span>
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
      <SelectFieldWithCreate
        label="Instructions Prompt"
        value={decl.instructions}
        onChange={(v) => updateHuman(decl.name, { instructions: v })}
        options={promptOptions}
        allowEmpty
        emptyLabel="-- select prompt --"
        onCreate={createPrompt}
      />
      <SelectField
        label="Mode"
        value={decl.mode}
        onChange={(v) => updateHuman(decl.name, { mode: v as HumanMode })}
        options={[
          { value: "pause_until_answers", label: "pause_until_answers" },
          { value: "auto_answer", label: "auto_answer" },
          { value: "auto_or_pause", label: "auto_or_pause" },
        ]}
        help="pause_until_answers = always wait for human input; auto_answer = LLM generates answer (requires model); auto_or_pause = LLM decides whether to answer or pause."
      />
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
        options={[
          { value: "none", label: "none" },
          { value: "wait_all", label: "wait_all" },
          { value: "best_effort", label: "best_effort" },
        ]}
        help="Implicit convergence: wait_all = wait for all incoming branches; best_effort = continue when available results are ready; none = no await (default)."
      />
      {needsModel && (
        <div className="border-t border-gray-700 pt-2 mt-2">
          <p className="text-[10px] text-yellow-400 mb-1">Required for {decl.mode} mode</p>
        </div>
      )}
      <TextField
        label="Model"
        value={decl.model ?? ""}
        onChange={(v) => updateHuman(decl.name, { model: v || undefined })}
        placeholder={needsModel ? "e.g. ${ANTHROPIC_MODEL}" : "Optional model override"}
      />
      <SelectFieldWithCreate
        label="System Prompt"
        value={decl.system ?? ""}
        onChange={(v) => updateHuman(decl.name, { system: v || undefined })}
        options={promptOptions}
        allowEmpty
        emptyLabel="-- select prompt --"
        onCreate={createPrompt}
      />
    </div>
  );
}
