import { useDocumentStore } from "@/store/document";
import type { ToolNodeDecl } from "@/api/types";
import { TextField, SelectField } from "./FormField";

interface Props {
  decl: ToolNodeDecl;
}

export default function ToolForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateTool = useDocumentStore((s) => s.updateTool);
  const renameNode = useDocumentStore((s) => s.renameNode);

  const schemaOptions = (document?.schemas ?? []).map((s) => ({ value: s.name, label: s.name }));

  return (
    <div className="space-y-1">
      <div
        className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
        style={{ backgroundColor: "#8B691422", borderLeft: "3px solid #8B6914" }}
      >
        <span className="text-base">{"\u{1F527}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#8B6914" }}>Tool</span>
      </div>
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <TextField
        label="Command"
        value={decl.command}
        onChange={(v) => updateTool(decl.name, { command: v })}
        placeholder="Shell command (e.g. ${CMD})"
      />
      <SelectField
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateTool(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
      />
    </div>
  );
}
