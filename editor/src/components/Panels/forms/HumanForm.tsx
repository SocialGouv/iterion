import { useDocumentStore } from "@/store/document";
import type { HumanDecl, HumanMode } from "@/api/types";
import { TextField, NumberField, SelectField } from "./FormField";

interface Props {
  decl: HumanDecl;
}

export default function HumanForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateHuman = useDocumentStore((s) => s.updateHuman);
  const renameNode = useDocumentStore((s) => s.renameNode);

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));
  const promptOptions = (document?.prompts ?? []).map((p) => ({ value: p.name, label: p.name }));

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
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <SelectField
        label="Input Schema"
        value={decl.input}
        onChange={(v) => updateHuman(decl.name, { input: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
      />
      <SelectField
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateHuman(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
      />
      <TextField
        label="Publish"
        value={decl.publish ?? ""}
        onChange={(v) => updateHuman(decl.name, { publish: v || undefined })}
        placeholder="Artifact name"
      />
      <SelectField
        label="Instructions Prompt"
        value={decl.instructions}
        onChange={(v) => updateHuman(decl.name, { instructions: v })}
        options={promptOptions}
        allowEmpty
        emptyLabel="-- select prompt --"
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
      />
      <NumberField
        label="Min Answers"
        value={decl.min_answers}
        onChange={(v) => updateHuman(decl.name, { min_answers: v })}
        min={1}
      />
      {needsModel && (
        <div className="border-t border-gray-700 pt-2 mt-2">
          <p className="text-[10px] text-yellow-400 mb-1">Required for {decl.mode} mode</p>
          <TextField
            label="Model"
            value={decl.model ?? ""}
            onChange={(v) => updateHuman(decl.name, { model: v || undefined })}
            placeholder="e.g. ${ANTHROPIC_MODEL}"
          />
          <SelectField
            label="System Prompt"
            value={decl.system ?? ""}
            onChange={(v) => updateHuman(decl.name, { system: v || undefined })}
            options={promptOptions}
            allowEmpty
            emptyLabel="-- select prompt --"
          />
        </div>
      )}
      {!needsModel && (
        <>
          <TextField
            label="Model"
            value={decl.model ?? ""}
            onChange={(v) => updateHuman(decl.name, { model: v || undefined })}
            placeholder="Optional model override"
          />
          <SelectField
            label="System Prompt"
            value={decl.system ?? ""}
            onChange={(v) => updateHuman(decl.name, { system: v || undefined })}
            options={promptOptions}
            allowEmpty
            emptyLabel="-- select prompt --"
          />
        </>
      )}
    </div>
  );
}
