import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import type { JoinDecl, JoinStrategy } from "@/api/types";
import { defaultSchema, getAllNodeNames } from "@/lib/defaults";
import { CommittedTextField, SelectField, SelectFieldWithCreate, MultiSelectField } from "./FormField";

interface Props {
  decl: JoinDecl;
}

export default function JoinForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateJoin = useDocumentStore((s) => s.updateJoin);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const nodeNames = document ? Array.from(getAllNodeNames(document)).filter((n) => n !== decl.name) : [];
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
        style={{ backgroundColor: "#2ECC7122", borderLeft: "3px solid #2ECC71" }}
      >
        <span className="text-base">{"\u{1F91D}"}</span>
        <span className="text-xs font-bold uppercase tracking-wide" style={{ color: "#2ECC71" }}>Join</span>
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
        label="Strategy"
        value={decl.strategy}
        onChange={(v) => updateJoin(decl.name, { strategy: v as JoinStrategy })}
        options={[
          { value: "wait_all", label: "wait_all" },
          { value: "best_effort", label: "best_effort" },
        ]}
        help="wait_all = wait for all required branches to complete; best_effort = continue when available results are ready."
      />
      <MultiSelectField
        label="Require"
        values={decl.require ?? []}
        onChange={(v) => updateJoin(decl.name, { require: v })}
        options={nodeNames}
      />
      <SelectFieldWithCreate
        label="Output Schema"
        value={decl.output}
        onChange={(v) => updateJoin(decl.name, { output: v })}
        options={schemaOptions}
        allowEmpty
        emptyLabel="-- select schema --"
        onCreate={createSchema}
      />
    </div>
  );
}
