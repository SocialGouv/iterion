import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import type { JoinDecl, JoinStrategy } from "@/api/types";
import { defaultSchema } from "@/lib/defaults";
import { TextField, SelectField, SelectFieldWithCreate, MultiSelectField } from "./FormField";
import { getAllNodeNames } from "@/lib/defaults";

interface Props {
  decl: JoinDecl;
}

export default function JoinForm({ decl }: Props) {
  const document = useDocumentStore((s) => s.document);
  const updateJoin = useDocumentStore((s) => s.updateJoin);
  const renameNode = useDocumentStore((s) => s.renameNode);
  const addSchema = useDocumentStore((s) => s.addSchema);

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
      <TextField
        label="Name"
        value={decl.name}
        onChange={(v) => renameNode(decl.name, v)}
      />
      <SelectField
        label="Strategy"
        value={decl.strategy}
        onChange={(v) => updateJoin(decl.name, { strategy: v as JoinStrategy })}
        options={[
          { value: "wait_all", label: "wait_all" },
          { value: "best_effort", label: "best_effort" },
        ]}
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
