import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import type { ToolNodeDecl } from "@/api/types";
import { defaultSchema, getAllNodeNames } from "@/lib/defaults";
import { TextField, CommittedTextField, SelectFieldWithCreate } from "./FormField";

interface Props {
  decl: ToolNodeDecl;
}

export default function ToolForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateTool = useDocumentStore((s) => s.updateTool);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));

  const createSchema = useCallback(() => {
    const existing = new Set((document?.schemas ?? []).map((s) => s.name));
    let i = 1;
    while (existing.has(`schema_${i}`)) i++;
    const name = `schema_${i}`;
    addSchema(defaultSchema(name));
    return name;
  }, [document, addSchema]);

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#8B691422", borderLeft: "3px solid #8B6914" }}
      >
        <span className="text-base">{"\u{1F527}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#8B6914" }}>Tool</span>
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
      <TextField
        label="Command"
        value={decl.command}
        onChange={(v) => updateTool(decl.name, { command: v })}
        placeholder="Shell command (e.g. ${CMD})"
        help="Shell command to execute. Use ${ENV_VAR} for environment variable substitution."
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateTool(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
    </div>
  );
}
